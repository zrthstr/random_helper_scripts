// gh-mass-clone clones (or updates) every repository of a GitHub user or org.
//
// MIT License
// Copyright (c) 2026, zrthstr
//
// Stdlib only, so `go build` needs no network and the result is a single
// static binary you can scp onto a box that has neither python nor requests.
//
// Usage:
//
//	gh-mass-clone -list-only some-org
//	gh-mass-clone -dest ~/backup -mirror some-org
//	gh-mass-clone -no-forks -no-archived -jobs 8 some-org
//
// Auth: GITHUB_TOKEN or GH_TOKEN, or -token-file, or whatever `gh auth token`
// hands back. Deliberately no -token flag: argv is world-readable via /proc.
//
// The token reaches git as an http.extraheader through GIT_CONFIG_* env vars
// rather than being baked into the remote URL, so it never lands in
// .git/config and never shows up in `ps`.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const api = "https://api.github.com"

// version is stamped in at build time by the Makefile (-ldflags -X).
var version = "dev"

type repo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	SSHURL   string `json:"ssh_url"`
	Private  bool   `json:"private"`
	Fork     bool   `json:"fork"`
	Archived bool   `json:"archived"`
}

type opts struct {
	dest       string
	tokenFile  string
	ssh        bool
	mirror     bool
	shallow    bool
	noForks    bool
	noArchived bool
	jobs       int
	listOnly   bool
	dryRun     bool
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}

func getToken(tokenFile string) string {
	if tokenFile != "" {
		b, err := os.ReadFile(expand(tokenFile))
		if err != nil {
			die("reading token file: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	for _, v := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if s := strings.TrimSpace(os.Getenv(v)); s != "" {
			return s
		}
	}
	// fall back to whatever the gh cli already has
	out, err := exec.Command("gh", "auth", "token").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

type client struct {
	http  *http.Client
	token string
}

func (c *client) do(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "gh-mass-clone")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.http.Do(req)
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// paginate walks the Link: rel=next chain. found is false when the endpoint
// 404s, which is how callers tell "not an org" apart from "org with no repos".
func (c *client) paginate(url string) (out []repo, found bool) {
	for url != "" {
		resp, err := c.do(url)
		if err != nil {
			die("api request: %v", err)
		}
		if resp.StatusCode == 403 && resp.Header.Get("X-RateLimit-Remaining") == "0" {
			resp.Body.Close()
			reset, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
			wait := time.Until(time.Unix(reset, 0)) + 2*time.Second
			if wait > 0 {
				fmt.Fprintf(os.Stderr, "rate limited, sleeping %v\n", wait.Round(time.Second))
				time.Sleep(wait)
			}
			continue
		}
		if resp.StatusCode == 404 {
			resp.Body.Close()
			return nil, false
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			die("api returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		var page []repo
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			die("decoding api response: %v", err)
		}
		link := resp.Header.Get("Link")
		resp.Body.Close()
		out = append(out, page...)

		url = ""
		if m := linkNext.FindStringSubmatch(link); m != nil {
			url = m[1]
		}
	}
	return out, true
}

func (c *client) whoami() string {
	if c.token == "" {
		return ""
	}
	resp, err := c.do(api + "/user")
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	var u struct {
		Login string `json:"login"`
	}
	json.NewDecoder(resp.Body).Decode(&u)
	return u.Login
}

// listRepos picks among three endpoints that GitHub treats differently:
//
//	/user/repos    - the only one that returns YOUR OWN private repos
//	/orgs/X/repos  - org repos, private ones included if the token can see them
//	/users/X/repos - public repos only, always, even with a token
func (c *client) listRepos(owner string) []repo {
	if c.token != "" && strings.EqualFold(owner, c.whoami()) {
		fmt.Printf("[*] %s is the authenticated user, using /user/repos (includes private)\n", owner)
		r, _ := c.paginate(api + "/user/repos?per_page=100&affiliation=owner&type=all")
		return r
	}
	if r, found := c.paginate(api + "/orgs/" + owner + "/repos?per_page=100&type=all"); found {
		fmt.Printf("[*] %s resolved as an org\n", owner)
		return r
	}
	r, found := c.paginate(api + "/users/" + owner + "/repos?per_page=100&type=owner")
	if !found {
		die("no such user or org: %s", owner)
	}
	fmt.Printf("[*] %s resolved as a user (public repos only)\n", owner)
	return r
}

// gitEnv injects credentials via env so they never touch disk or argv.
func gitEnv(token string) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // fail, don't hang on a prompt
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
		)
	}
	return env
}

func runGit(env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = env
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stderr.String()), err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

type result struct {
	name, status, detail string
}

func syncOne(r repo, dest string, env []string, o opts) result {
	url := r.CloneURL
	if o.ssh {
		url = r.SSHURL
	}
	name := r.Name
	if o.mirror {
		name += ".git"
	}
	path := filepath.Join(dest, name)

	if o.dryRun {
		return result{r.Name, "would clone", url}
	}

	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		var out string
		if o.mirror {
			out, err = runGit(env, path, "remote", "update", "--prune")
		} else {
			out, err = runGit(env, path, "fetch", "--all", "--prune", "--tags")
		}
		if err != nil {
			return result{r.Name, "fetch failed", lastLine(out)}
		}
		if !o.mirror {
			// only fast-forward; never clobber local work
			runGit(env, path, "merge", "--ff-only", "@{u}")
		}
		return result{r.Name, "updated", ""}
	}

	args := []string{"clone"}
	switch {
	case o.mirror:
		args = append(args, "--mirror")
	case o.shallow:
		args = append(args, "--depth", "1")
	}
	args = append(args, url, path)
	if out, err := runGit(env, "", args...); err != nil {
		return result{r.Name, "clone failed", lastLine(out)}
	}
	return result{r.Name, "cloned", ""}
}

func main() {
	var o opts
	flag.StringVar(&o.dest, "dest", ".", "destination dir; repos land in <dest>/<owner>/")
	flag.StringVar(&o.tokenFile, "token-file", "", "read token from this file instead of the environment")
	flag.BoolVar(&o.ssh, "ssh", false, "clone over SSH instead of HTTPS+token")
	flag.BoolVar(&o.mirror, "mirror", false, "bare --mirror clones (what you want for backups)")
	flag.BoolVar(&o.shallow, "shallow", false, "--depth 1 (ignored with -mirror)")
	flag.BoolVar(&o.noForks, "no-forks", false, "skip forks")
	flag.BoolVar(&o.noArchived, "no-archived", false, "skip archived repos")
	flag.IntVar(&o.jobs, "jobs", 4, "parallel git processes")
	flag.BoolVar(&o.listOnly, "list-only", false, "print the repo list and exit")
	flag.BoolVar(&o.dryRun, "dry-run", false, "show what would happen, clone nothing")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <user-or-org>\n\nflags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("gh-mass-clone %s\n", version)
		return
	}

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	owner := flag.Arg(0)

	token := getToken(o.tokenFile)
	if token == "" {
		fmt.Fprintln(os.Stderr, "[!] no token found; only public repos will be visible")
	}

	c := &client{http: &http.Client{Timeout: 30 * time.Second}, token: token}
	repos := c.listRepos(owner)

	filtered := repos[:0]
	for _, r := range repos {
		if o.noForks && r.Fork {
			continue
		}
		if o.noArchived && r.Archived {
			continue
		}
		filtered = append(filtered, r)
	}
	repos = filtered
	sort.Slice(repos, func(i, j int) bool {
		return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name)
	})

	fmt.Printf("[*] %d repos\n", len(repos))
	if o.listOnly {
		for _, r := range repos {
			var flags []string
			if r.Private {
				flags = append(flags, "private")
			}
			if r.Fork {
				flags = append(flags, "fork")
			}
			if r.Archived {
				flags = append(flags, "archived")
			}
			line := "  " + r.FullName
			if len(flags) > 0 {
				line += " [" + strings.Join(flags, ",") + "]"
			}
			fmt.Println(line)
		}
		return
	}

	dest := filepath.Join(expand(o.dest), owner)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		die("creating %s: %v", dest, err)
	}
	gitToken := token
	if o.ssh {
		gitToken = "" // ssh clones authenticate with the key, not the token
	}
	env := gitEnv(gitToken)

	// jobs stays low by default: GitHub throttles bursts of clones via a
	// secondary rate limit that does not announce itself in the headers.
	if o.jobs < 1 {
		o.jobs = 1
	}
	sem := make(chan struct{}, o.jobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, failed int

	for _, r := range repos {
		wg.Add(1)
		go func(r repo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := syncOne(r, dest, env, o)

			mu.Lock()
			defer mu.Unlock()
			fmt.Printf("  %12s  %s  %s\n", res.status, res.name, res.detail)
			if strings.Contains(res.status, "failed") {
				failed++
			} else {
				ok++
			}
		}(r)
	}
	wg.Wait()

	fmt.Printf("[*] done: %d ok, %d failed -> %s\n", ok, failed, dest)
	if failed > 0 {
		os.Exit(1)
	}
}
