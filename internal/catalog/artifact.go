package catalog

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

const maxTarballBytes = 200 << 20

// Artifact is a verified, owner-only temporary file. The caller must remove it
// after building a pinned runtime image; this function never installs a tool.
type Artifact struct {
	Path   string
	SHA256 string
	Size   int64
}

// DownloadVerified checks an exact npm release against its SHA-512 SRI value
// before returning any file to a trusted runtime builder.
func DownloadVerified(ctx context.Context, client *http.Client, tool string, release Release, dir string) (artifact Artifact, retErr error) {
	name, ok := packages[tool]
	if !ok || client == nil || dir == "" {
		return Artifact{}, errors.New("invalid artifact request")
	}
	if _, ok := stableParts(release.Version); !ok {
		return Artifact{}, errors.New("invalid release version")
	}
	u, err := url.Parse(release.Tarball)
	if err != nil || u.Scheme != "https" || u.Host != "registry.npmjs.org" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.String() != release.Tarball ||
		u.Path != "/"+name+"/-/"+path.Base(name)+"-"+release.Version+".tgz" {
		return Artifact{}, errors.New("unexpected release tarball URL")
	}
	algorithm, encoded, ok := strings.Cut(release.Integrity, "-")
	want, err := base64.StdEncoding.DecodeString(encoded)
	if !ok || algorithm != "sha512" || err != nil || len(want) != sha512.Size {
		return Artifact{}, errors.New("invalid release integrity")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, release.Tarball, nil)
	if err != nil {
		return Artifact{}, err
	}
	downloader := *client
	downloader.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := downloader.Do(request)
	if err != nil {
		return Artifact{}, fmt.Errorf("download release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Request == nil || response.Request.URL.String() != release.Tarball ||
		response.ContentLength > maxTarballBytes {
		return Artifact{}, errors.New("release download was redirected, oversized, or unavailable")
	}
	file, err := os.CreateTemp(dir, ".blaxsmith-*.tgz")
	if err != nil {
		return Artifact{}, fmt.Errorf("create artifact file: %w", err)
	}
	keep := false
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close artifact file: %w", err))
			keep = false
		}
		if !keep {
			if err := os.Remove(file.Name()); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("remove failed artifact: %w", err))
			}
		}
	}()
	hash512, hash256 := sha512.New(), sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash512, hash256), io.LimitReader(response.Body, maxTarballBytes+1))
	if err != nil {
		return Artifact{}, fmt.Errorf("read release: %w", err)
	}
	if size == 0 || size > maxTarballBytes || subtle.ConstantTimeCompare(hash512.Sum(nil), want) != 1 {
		return Artifact{}, errors.New("release integrity mismatch")
	}
	if err := file.Sync(); err != nil {
		return Artifact{}, fmt.Errorf("sync artifact file: %w", err)
	}
	keep = true
	return Artifact{Path: file.Name(), SHA256: hex.EncodeToString(hash256.Sum(nil)), Size: size}, nil
}
