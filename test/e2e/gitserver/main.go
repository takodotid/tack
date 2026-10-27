// Command gitserver serves bare Git repositories over smart HTTP, with push
// enabled, through git http-backend. It is for end-to-end tests only: it has
// no authentication.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	root := flag.String("root", "/srv/git", "directory that holds the bare repositories")
	backend := flag.String("backend", "/usr/libexec/git-core/git-http-backend", "path to git http-backend")
	repos := flag.String("repos", "gitops", "comma-separated bare repositories to create when missing")
	flag.Parse()

	for _, name := range strings.Split(*repos, ",") {
		if name == "" {
			continue
		}
		if err := ensureRepo(filepath.Join(*root, name+".git")); err != nil {
			log.Fatalf("create repository %s: %v", name, err)
		}
	}

	backendHandler := &cgi.Handler{
		Path: *backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + *root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.RequestURI())
		backendHandler.ServeHTTP(w, r)
	}))
	log.Printf("serving %s on %s", *root, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// ensureRepo creates a bare repository with main as its default branch and
// anonymous push enabled, unless it already exists.
func ensureRepo(path string) error {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		return nil
	}
	steps := [][]string{
		{"git", "init", "--quiet", "--bare", "--initial-branch=main", path},
		{"git", "-C", path, "config", "http.receivepack", "true"},
	}
	for _, args := range steps {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	return nil
}
