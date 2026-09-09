package drive

import (
	"context"
	"fmt"
	"strings"
	"sync"

	drivev3 "google.golang.org/api/drive/v3"
)

const folderMime = "application/vnd.google-apps.folder"

// folderCache maps a slash separated path to a Drive folder id.
//
// Drive has no paths, only parent links, so every segment costs a lookup.
// The cache is also what makes concurrent uploads safe: folder creation runs
// under one lock, otherwise two workers finishing at the same moment would
// each create their own "2014" and the archive would split in half.
type folderCache struct {
	mu    sync.Mutex
	byKey map[string]string
}

func newFolderCache(rootID string) *folderCache {
	return &folderCache{byKey: map[string]string{"": rootID}}
}

func (c *folderCache) root() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byKey[""]
}

// ensurePath resolves a path, creating the folders it does not find.
func (s *Store) ensurePath(ctx context.Context, path string) (string, error) {
	return s.resolvePath(ctx, path, true)
}

// lookupPath resolves a path without creating anything, returning "" when a
// segment is missing.
func (s *Store) lookupPath(ctx context.Context, path string) (string, error) {
	return s.resolvePath(ctx, path, false)
}

func (s *Store) resolvePath(ctx context.Context, path string, create bool) (string, error) {
	segments := splitPath(path)

	s.folders.mu.Lock()
	defer s.folders.mu.Unlock()

	parent := s.folders.byKey[""]
	key := ""
	for _, name := range segments {
		if key == "" {
			key = name
		} else {
			key += "/" + name
		}
		if id, ok := s.folders.byKey[key]; ok {
			parent = id
			continue
		}

		id, err := s.findFolder(ctx, name, parent)
		if err != nil {
			return "", err
		}
		if id == "" {
			if !create {
				return "", nil
			}
			if id, err = s.createFolder(ctx, name, parent); err != nil {
				return "", err
			}
		}
		s.folders.byKey[key] = id
		parent = id
	}
	return parent, nil
}

func splitPath(path string) []string {
	raw := strings.Split(strings.Trim(path, "/"), "/")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) findFolder(ctx context.Context, name, parent string) (string, error) {
	q := fmt.Sprintf("name = '%s' and mimeType = '%s' and '%s' in parents and trashed = false",
		escapeQuery(name), folderMime, escapeQuery(parent))

	call := s.svc.Files.List().
		Q(q).
		Fields("files(id, name)").
		PageSize(2).
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true)
	if s.sharedDriveID != "" {
		call = call.Corpora("drive").DriveId(s.sharedDriveID)
	}

	res, err := call.Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("cannot look up folder %q: %w", name, err)
	}
	if len(res.Files) == 0 {
		return "", nil
	}
	return res.Files[0].Id, nil
}

func (s *Store) createFolder(ctx context.Context, name, parent string) (string, error) {
	f := &drivev3.File{
		Name:     name,
		MimeType: folderMime,
		Parents:  []string{parent},
	}
	created, err := s.svc.Files.Create(f).
		Fields("id").
		SupportsAllDrives(true).
		Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("cannot create folder %q: %w", name, err)
	}
	return created.Id, nil
}

// escapeQuery escapes a value for a Drive query string, where the only
// special characters are the backslash and the single quote.
func escapeQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

// findFile returns the id and size of a file inside a folder, or an empty id
// when it is not there.
func (s *Store) findFile(ctx context.Context, name, parent string) (id string, size int64, err error) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false",
		escapeQuery(name), escapeQuery(parent))

	call := s.svc.Files.List().
		Q(q).
		Fields("files(id, name, size)").
		PageSize(2).
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true)
	if s.sharedDriveID != "" {
		call = call.Corpora("drive").DriveId(s.sharedDriveID)
	}

	res, err := call.Context(ctx).Do()
	if err != nil {
		return "", 0, fmt.Errorf("cannot look up file %q: %w", name, err)
	}
	if len(res.Files) == 0 {
		return "", 0, nil
	}
	return res.Files[0].Id, res.Files[0].Size, nil
}

// listFolder sums the sizes of the files directly inside a folder.
func (s *Store) listFolder(ctx context.Context, folderID string) (count int, bytes int64, err error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", escapeQuery(folderID))
	token := ""
	for {
		call := s.svc.Files.List().
			Q(q).
			Fields("nextPageToken, files(id, size, mimeType)").
			PageSize(200).
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true)
		if s.sharedDriveID != "" {
			call = call.Corpora("drive").DriveId(s.sharedDriveID)
		}
		if token != "" {
			call = call.PageToken(token)
		}

		res, err := call.Context(ctx).Do()
		if err != nil {
			return 0, 0, fmt.Errorf("cannot list folder contents: %w", err)
		}
		for _, f := range res.Files {
			if f.MimeType == folderMime {
				continue
			}
			count++
			bytes += f.Size
		}
		if res.NextPageToken == "" {
			return count, bytes, nil
		}
		token = res.NextPageToken
	}
}
