// rcboard: a tailnet-only web page that starts and stops one
// `claude remote-control` server per project directory, each inside a
// detached tmux session named rc-<project>. The Claude mobile app handles
// everything below that level (listing and creating sessions inside a
// running server), so this page only manages the servers themselves.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML embed.FS

var tmpl = template.Must(template.ParseFS(indexHTML, "index.html"))

// Project names double as tmux session names; keep them shell- and tmux-safe.
var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

const sessionPrefix = "rc-"

type Project struct {
	Name    string
	Dir     string
	Git     bool
	Running bool
	Since   string // tmux session creation time, human form
	Workers string // "used/max" capacity as printed by the server, "" if unknown
	Tail    string // last lines of the tmux pane, for debugging from the phone
	LastLog string // pane contents captured when the server last exited
}

type app struct {
	root      string
	claudeCfg string // ~/.claude.json, holds per-dir trust acceptance
	logDir    string // per-project capture of the pane at server exit
}

func main() {
	home, _ := os.UserHomeDir()
	root := flag.String("root", filepath.Join(home, "code"), "directory whose subdirectories are projects")
	listen := flag.String("listen", "127.0.0.1:7777", "listen address; front with `tailscale serve`")
	flag.Parse()

	a := &app{
		root:      *root,
		claudeCfg: filepath.Join(home, ".claude.json"),
		logDir:    filepath.Join(home, ".cache", "rcboard"),
	}
	if err := os.MkdirAll(a.logDir, 0o700); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("POST /start", sameOrigin(a.start))
	mux.HandleFunc("POST /stop", sameOrigin(a.stop))
	log.Printf("rcboard listening on %s, root %s", *listen, a.root)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	projects, err := a.projects()
	if err != nil {
		http.Error(w, "listing projects failed", http.StatusInternalServerError)
		log.Print(err)
		return
	}
	err = tmpl.Execute(w, map[string]any{
		"Projects": projects,
		"Root":     a.root,
		"Error":    r.URL.Query().Get("err"),
	})
	if err != nil {
		log.Print(err)
	}
}

func (a *app) start(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	spawn := r.FormValue("spawn")
	if spawn != "same-dir" && spawn != "worktree" {
		spawn = "same-dir"
	}
	dir, err := a.projectDir(name)
	if err != nil {
		redirectErr(w, r, err)
		return
	}
	if !a.trustedDirs()[dir] {
		if err := a.trust(dir); err != nil {
			redirectErr(w, r, fmt.Errorf("recording workspace trust: %w", err))
			return
		}
	}
	session := sessionPrefix + name
	// `claude remote-control` is a TUI and wants a pty, hence tmux rather than a
	// plain detached process. The session name doubles as the Remote Control
	// name so the pre-created session is recognizable in the mobile app.
	// The bash wrapper saves the pane when claude exits, so a server that dies
	// (bad flag, auth expired, trust revoked) leaves its last words on the page.
	wrapper := `claude remote-control "$@"; tmux capture-pane -p -t "$TMUX_PANE" > "$RCBOARD_LOG"`
	cmd := exec.Command("tmux", "new-session", "-d", "-s", session, "-c", dir,
		"-e", "RCBOARD_LOG="+a.logPath(name),
		"--", "bash", "-c", wrapper, "rcboard",
		"--name", name,
		"--permission-mode", "auto",
		"--spawn", spawn)
	if out, err := cmd.CombinedOutput(); err != nil {
		redirectErr(w, r, fmt.Errorf("tmux: %s", strings.TrimSpace(string(out))))
		return
	}
	// Confirm the server outlived its startup checks before reporting success.
	time.Sleep(2 * time.Second)
	if _, alive := tmuxSessions()[session]; !alive {
		redirectErr(w, r, fmt.Errorf("%s exited at startup: %s", name, lastLine(a.lastLog(name))))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) logPath(name string) string { return filepath.Join(a.logDir, name+".log") }

func (a *app) lastLog(name string) string {
	raw, err := os.ReadFile(a.logPath(name))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(raw), "\n ")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "no output captured"
}

func (a *app) stop(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if _, err := a.projectDir(name); err != nil {
		redirectErr(w, r, err)
		return
	}
	// Killing the tmux session SIGHUPs the claude server; it has no drain
	// worth waiting for, and any live session is reattachable from the app.
	cmd := exec.Command("tmux", "kill-session", "-t", "="+sessionPrefix+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		redirectErr(w, r, fmt.Errorf("tmux: %s", strings.TrimSpace(string(out))))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sameOrigin rejects cross-site form posts. The tailnet is the auth layer,
// but any web page open on a tailnet device could otherwise POST here.
// Browsers always send Sec-Fetch-Site; its absence means a non-browser client.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func redirectErr(w http.ResponseWriter, r *http.Request, err error) {
	http.Redirect(w, r, "/?err="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
}

// projectDir validates a user-supplied name and resolves it under root.
func (a *app) projectDir(name string) (string, error) {
	if !safeName.MatchString(name) || name == "." || name == ".." {
		return "", errors.New("bad project name")
	}
	dir := filepath.Join(a.root, name)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", errors.New("no such project")
	}
	return dir, nil
}

func (a *app) projects() ([]Project, error) {
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return nil, err
	}
	running := tmuxSessions()

	var out []Project
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || !safeName.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(a.root, e.Name())
		p := Project{
			Name: e.Name(),
			Dir:  dir,
		}
		_, gitErr := os.Stat(filepath.Join(dir, ".git"))
		p.Git = gitErr == nil
		if created, ok := running[sessionPrefix+e.Name()]; ok {
			p.Running = true
			p.Since = created.Local().Format("Mon 15:04")
			pane := paneText(sessionPrefix + e.Name())
			p.Tail = tail(pane, 6)
			p.Workers = capacity(pane)
		} else {
			p.LastLog = a.lastLog(e.Name())
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// trustedDirs reads the workspace-trust map from ~/.claude.json. A server
// started in an untrusted dir blocks on a dialog nobody can see, so the page
// refuses to start those and says why.
func (a *app) trustedDirs() map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(a.claudeCfg)
	if err != nil {
		return out
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return out
	}
	for dir, p := range cfg.Projects {
		out[dir] = p.HasTrustDialogAccepted
	}
	return out
}

// trust records workspace trust for dir in ~/.claude.json, the same thing the
// interactive dialog does. Everything else in the file is preserved as-is and
// the write is an atomic rename so a running claude never reads a torn file.
// There is no env or flag override for the dialog in the CLI; this is it.
func (a *app) trust(dir string) error {
	raw, err := os.ReadFile(a.claudeCfg)
	if err != nil {
		return err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	projects := map[string]json.RawMessage{}
	if p, ok := cfg["projects"]; ok {
		if err := json.Unmarshal(p, &projects); err != nil {
			return err
		}
	}
	entry := map[string]any{}
	if e, ok := projects[dir]; ok {
		if err := json.Unmarshal(e, &entry); err != nil {
			return err
		}
	}
	entry["hasTrustDialogAccepted"] = true
	if projects[dir], err = json.Marshal(entry); err != nil {
		return err
	}
	if cfg["projects"], err = json.Marshal(projects); err != nil {
		return err
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.claudeCfg + ".rcboard.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.claudeCfg)
}

// tmuxSessions returns name -> creation time for every tmux session.
func tmuxSessions() map[string]time.Time {
	out := map[string]time.Time{}
	raw, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}\t#{session_created}").Output()
	if err != nil {
		return out // no server running means no sessions
	}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		name, ts, ok := strings.Cut(string(line), "\t")
		if !ok {
			continue
		}
		var unix int64
		fmt.Sscan(ts, &unix)
		out[name] = time.Unix(unix, 0)
	}
	return out
}

func paneText(session string) string {
	raw, err := exec.Command("tmux", "capture-pane", "-p", "-t", session+":").Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(raw), "\n ")
}

func tail(s string, lines int) string {
	all := strings.Split(s, "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// The server's status line reads "Capacity: 1/32 · New sessions will ...".
var capacityRe = regexp.MustCompile(`Capacity: (\d+/\d+)`)

func capacity(pane string) string {
	m := capacityRe.FindStringSubmatch(pane)
	if m == nil {
		return ""
	}
	return m[1]
}
