// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"net/url"
	"path"
	"strings"

	"code.gitea.io/gitea/models/renderhelper"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/charset"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/markup"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/context"
)

// locate a README for a tree in one of the supported paths.
//
// entries is passed to reduce calls to ListEntries(), so
// this has precondition:
//
//	entries == ctx.Repo.Commit.SubTree(ctx.Repo.TreePath).ListEntries()
//
// FIXME: There has to be a more efficient way of doing this
func findReadmeFileInEntries(ctx *context.Context, parentDir string, entries []*git.TreeEntry, tryWellKnownDirs bool) (string, *git.TreeEntry, error) {
	docsEntries := make([]*git.TreeEntry, 3) // (one of docs/, .gitea/ or .github/)
	for _, entry := range entries {
		if tryWellKnownDirs && entry.IsDir() {
			// as a special case for the top-level repo introduction README,
			// fall back to subfolders, looking for e.g. docs/README.md, .gitea/README.zh-CN.txt, .github/README.txt, ...
			// (note that docsEntries is ignored unless we are at the root)
			lowerName := strings.ToLower(entry.Name())
			switch lowerName {
			case "docs":
				if entry.Name() == "docs" || docsEntries[0] == nil {
					docsEntries[0] = entry
				}
			case ".gitea":
				if entry.Name() == ".gitea" || docsEntries[1] == nil {
					docsEntries[1] = entry
				}
			case ".github":
				if entry.Name() == ".github" || docsEntries[2] == nil {
					docsEntries[2] = entry
				}
			}
		}
	}

	// Create a list of extensions in priority order
	// 1. Markdown files - with and without localisation - e.g. README.en-us.md or README.md
	// 2. Txt files - e.g. README.txt
	// 3. No extension - e.g. README
	exts := append(localizedExtensions(".md", ctx.Locale.Language()), ".txt", "") // sorted by priority
	extCount := len(exts)
	readmeFiles := make([]*git.TreeEntry, extCount+1)
	for _, entry := range entries {
		if i, ok := util.IsReadmeFileExtension(entry.Name(), exts...); ok {
			fullPath := path.Join(parentDir, entry.Name())
			if readmeFiles[i] == nil || base.NaturalSortLess(readmeFiles[i].Name(), entry.Blob().Name()) {
				if entry.IsLink() {
					res, err := git.EntryFollowLinks(ctx.Repo.Commit, fullPath, entry)
					if err == nil && (res.TargetEntry.IsExecutable() || res.TargetEntry.IsRegular()) {
						readmeFiles[i] = entry
					}
				} else {
					readmeFiles[i] = entry
				}
			}
		}
	}

	var readmeFile *git.TreeEntry
	for _, f := range readmeFiles {
		if f != nil {
			readmeFile = f
			break
		}
	}

	if ctx.Repo.TreePath == "" && readmeFile == nil {
		for _, subTreeEntry := range docsEntries {
			if subTreeEntry == nil {
				continue
			}
			subTree := subTreeEntry.Tree()
			if subTree == nil {
				// this should be impossible; if subTreeEntry exists so should this.
				continue
			}
			childEntries, err := subTree.ListEntries()
			if err != nil {
				return "", nil, err
			}

			subfolder, readmeFile, err := findReadmeFileInEntries(ctx, parentDir, childEntries, false)
			if err != nil && !git.IsErrNotExist(err) {
				return "", nil, err
			}
			if readmeFile != nil {
				return path.Join(subTreeEntry.Name(), subfolder), readmeFile, nil
			}
		}
	}

	return "", readmeFile, nil
}

// localizedExtensions prepends the provided language code with and without a
// regional identifier to the provided extension.
// Note: the language code will always be lower-cased, if a region is present it must be separated with a `-`
// Note: ext should be prefixed with a `.`
func localizedExtensions(ext, languageCode string) (localizedExts []string) {
	if len(languageCode) < 1 {
		return []string{ext}
	}

	lowerLangCode := "." + strings.ToLower(languageCode)

	if strings.Contains(lowerLangCode, "-") {
		underscoreLangCode := strings.ReplaceAll(lowerLangCode, "-", "_")
		indexOfDash := strings.Index(lowerLangCode, "-")
		// e.g. [.zh-cn.md, .zh_cn.md, .zh.md, _zh.md, .md]
		return []string{lowerLangCode + ext, underscoreLangCode + ext, lowerLangCode[:indexOfDash] + ext, "_" + lowerLangCode[1:indexOfDash] + ext, ext}
	}

	// e.g. [.en.md, .md]
	return []string{lowerLangCode + ext, ext}
}

func prepareToRenderReadmeFile(ctx *context.Context, subfolder string, readmeFile *git.TreeEntry) {
	if readmeFile == nil {
		return
	}

	readmeFullPath := path.Join(ctx.Repo.TreePath, subfolder, readmeFile.Name())
	readmeTargetEntry := readmeFile
	if readmeFile.IsLink() {
		if res, err := git.EntryFollowLinks(ctx.Repo.Commit, readmeFullPath, readmeFile); err == nil {
			readmeTargetEntry = res.TargetEntry
		} else {
			readmeTargetEntry = nil // if we cannot resolve the symlink, we cannot render the readme, ignore the error
		}
	}
	if readmeTargetEntry == nil {
		return // if no valid README entry found, skip rendering the README
	}

	ctx.Data["RawFileLink"] = ""
	ctx.Data["ReadmeInList"] = path.Join(subfolder, readmeFile.Name()) // the relative path to the readme file to the current tree path
	ctx.Data["ReadmeExist"] = true
	ctx.Data["FileIsSymlink"] = readmeFile.IsLink()

	buf, dataRc, fInfo, err := getFileReader(ctx, ctx.Repo.Repository.ID, readmeTargetEntry.Blob())
	if err != nil {
		ctx.ServerError("getFileReader", err)
		return
	}
	defer dataRc.Close()

	ctx.Data["FileIsText"] = fInfo.st.IsText()
	ctx.Data["FileTreePath"] = readmeFullPath
	ctx.Data["FileSize"] = fInfo.fileSize
	ctx.Data["IsLFSFile"] = fInfo.isLFSFile()

	if fInfo.isLFSFile() {
		filenameBase64 := base64.RawURLEncoding.EncodeToString([]byte(readmeFile.Name()))
		ctx.Data["RawFileLink"] = fmt.Sprintf("%s.git/info/lfs/objects/%s/%s", ctx.Repo.Repository.Link(), url.PathEscape(fInfo.lfsMeta.Oid), url.PathEscape(filenameBase64))
	}

	if !fInfo.st.IsText() {
		return
	}

	if fInfo.fileSize >= setting.UI.MaxDisplayFileSize {
		// Pretend that this is a normal text file to display 'This file is too large to be shown'
		ctx.Data["IsFileTooLarge"] = true
		return
	}

	rd := charset.ToUTF8WithFallbackReader(io.MultiReader(bytes.NewReader(buf), dataRc), charset.ConvertOpts{})

	if markupType := markup.DetectMarkupTypeByFileName(readmeFile.Name()); markupType != "" {
		ctx.Data["IsMarkup"] = true
		ctx.Data["MarkupType"] = markupType

		rctx := renderhelper.NewRenderContextRepoFile(ctx, ctx.Repo.Repository, renderhelper.RepoFileOptions{
			CurrentRefPath:  ctx.Repo.RefTypeNameSubURL(),
			CurrentTreePath: path.Dir(readmeFullPath),
		}).
			WithMarkupType(markupType).
			WithRelativePath(readmeFullPath)

		ctx.Data["EscapeStatus"], ctx.Data["FileContent"], err = markupRender(ctx, rctx, rd)
		if err != nil {
			log.Error("Render failed for %s in %-v: %v Falling back to rendering source", readmeFile.Name(), ctx.Repo.Repository, err)
			delete(ctx.Data, "IsMarkup")
		}
	}

	if ctx.Data["IsMarkup"] != true {
		ctx.Data["IsPlainText"] = true
		content, err := io.ReadAll(rd)
		if err != nil {
			log.Error("Read readme content failed: %v", err)
		}
		contentEscaped := template.HTMLEscapeString(util.UnsafeBytesToString(content))
		ctx.Data["EscapeStatus"], ctx.Data["FileContent"] = charset.EscapeControlHTML(template.HTML(contentEscaped), ctx.Locale)
	}

	if !fInfo.isLFSFile() && ctx.Repo.Repository.CanEnableEditor() {
		ctx.Data["CanEditReadmeFile"] = true
	}
}
