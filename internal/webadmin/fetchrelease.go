package webadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/upgrade"
)

// The newest release fetched from GitHub, for Upgrade ▸ "Fetch from online". It finds the tag the way a person
// would: the newest published release, otherwise the highest v<number> tag, and downloads that tag's source archive
// into the same spool file an uploaded archive lands in, so everything after it (version read, build, preflight, the
// confirm-or-rollback guard, push to peers) is the path an upload takes. Nothing the signed-in user types reaches the
// request: the repository is fixed here. The variables exist only so tests can point them at a fake.
var (
	ghRepo = "micush/gravinet"
	ghAPI  = "https://api.github.com"
	ghHost = "https://github.com"
)

const fetchTimeout = 5 * time.Minute

var (
	ghTagRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	ghNumTagRe = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)*$`)
)

func ghRequest(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gravinet")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	return resp, nil
}

func ghGetSmall(ctx context.Context, url string, limit int64) ([]byte, error) {
	resp, err := ghRequest(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("GitHub's answer is too large")
	}
	return b, nil
}

// numericTagLess orders v9 < v10 and 1.2 < 1.10.
func numericTagLess(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// candidateTags lists the tags to try, best first: the newest published release, then the v<number> tags from the
// highest down (at most 10). More than one is returned because a repository's tags can include ones that are not
// releases of this project; the caller skips those.
func candidateTags(ctx context.Context) ([]string, error) {
	var out []string
	if b, err := ghGetSmall(ctx, ghAPI+"/repos/"+ghRepo+"/releases/latest", 1<<20); err == nil {
		var r struct {
			Tag string `json:"tag_name"`
		}
		if json.Unmarshal(b, &r) == nil && r.Tag != "" && ghTagRe.MatchString(r.Tag) {
			out = append(out, r.Tag)
		}
	}
	b, err := ghGetSmall(ctx, ghAPI+"/repos/"+ghRepo+"/tags?per_page=100", 4<<20)
	var tags []struct {
		Name string `json:"name"`
	}
	if err == nil {
		if jerr := json.Unmarshal(b, &tags); jerr != nil {
			err = errors.New("GitHub's answer was not understood")
		}
	}
	if err != nil {
		if len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	var nums []string
	for _, t := range tags {
		if ghNumTagRe.MatchString(t.Name) && (len(out) == 0 || t.Name != out[0]) {
			nums = append(nums, t.Name)
		}
	}
	sort.Slice(nums, func(i, j int) bool { return numericTagLess(nums[j], nums[i]) })
	out = append(out, nums...)
	if len(out) > 10 {
		out = out[:10]
	}
	if len(out) == 0 {
		return nil, errors.New("no release or v<number> tag found in " + ghRepo + " (rate limit or no tags yet)")
	}
	return out, nil
}

// fetchRelease downloads the newest tag's source archive into a spool file under dir and returns its path, digest
// and tag. A tag whose archive does not read as a gravinet source tree (no cmd/gravinet/main.go version) is skipped.
// The caller owns the returned path and must remove it.
func fetchRelease(ctx context.Context, dir string) (path, sum, tag string, err error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	cands, err := candidateTags(ctx)
	if err != nil {
		return "", "", "", err
	}
	var skipped []string
	for _, t := range cands {
		resp, err := ghRequest(ctx, ghHost+"/"+ghRepo+"/archive/refs/tags/"+t+".tar.gz")
		if err != nil {
			return "", "", t, fmt.Errorf("download of %s failed: %w", t, err)
		}
		p, s, err := spoolUpload(dir, resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", "", t, fmt.Errorf("download of %s failed: %w", t, err)
		}
		if f, oerr := os.Open(p); oerr == nil {
			v := upgrade.ExtractedVersion(f, dir)
			f.Close()
			if v != "" {
				return p, s, t, nil
			}
		}
		os.Remove(p)
		skipped = append(skipped, t)
	}
	return "", "", "", fmt.Errorf("none of the tags tried (%s) is a gravinet source tree", strings.Join(skipped, ", "))
}
