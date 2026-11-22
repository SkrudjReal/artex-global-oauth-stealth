package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

func TestReviewRestore(t *testing.T) {
	dir := t.TempDir()
	id := "conv-42;$(touch SHOULD_NOT_EXIST)`id`"
	opts := agentcore.Options{EnableWebFetch: true, WorkingDir: dir}
	if err := configureSessionHTTP(t.Context(), &opts, id); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, ".http-identity", "*", "headers.json"))
	if len(paths) != 1 {
		t.Fatal(paths)
	}
	before, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_UAFORGE_PYTHON", "/missing/python")
	restored := agentcore.Options{EnableWebFetch: true, WorkingDir: dir}
	if err := configureSessionHTTP(t.Context(), &restored, id); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(paths[0])
	if err != nil || string(before) != string(after) {
		t.Fatalf("restore: %v", err)
	}
	if len(restored.BashEnv) != 0 {
		t.Fatal("global CLI environment injected")
	}
	var expected map[string]string
	if err := json.Unmarshal(after, &expected); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != expected["User-Agent"] {
			t.Error("restored profile changed")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	input, _ := json.Marshal(map[string]string{"url": server.URL})
	fetch := restored.Tools[len(restored.Tools)-1]
	res, err := fetch.Call(t.Context(), input, &actool.ToolContext{})
	if err != nil || res.IsError {
		t.Fatalf("fetch: %v %s", err, res.Flatten())
	}
}

func TestReviewCurlWget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lab '$()`")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "INJECTED")
	ua := "Lab/42 $(touch " + marker + ") `id`"
	python := os.Getenv("ARTEX_UAFORGE_PYTHON")
	if python == "" {
		t.Fatal("ARTEX_UAFORGE_PYTHON is required")
	}
	fake := filepath.Join(dir, "python")
	data, _ := json.Marshal(map[string]string{"User-Agent": ua})
	script := "#!" + python + "\nprint(" + stringMustJSON(string(data)) + ")\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_UAFORGE_PYTHON", fake)
	opts := agentcore.Options{EnableWebFetch: true, WorkingDir: dir, BashEnv: []string{"UNCHANGED=yes"}}
	if err := configureSessionHTTP(t.Context(), &opts, "cli"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opts.BashEnv, []string{"UNCHANGED=yes"}) {
		t.Fatal("CLI defaults leak into other commands")
	}
	configs, _ := filepath.Glob(filepath.Join(dir, ".http-identity", "*", ".curlrc"))
	if len(configs) != 1 {
		t.Fatal(configs)
	}
	quoted, err := exec.Command("sh", "-c", "printf '%s' "+shellQuote(configs[0])).Output()
	if err != nil || string(quoted) != configs[0] {
		t.Fatalf("unsafe config path: %v %s", err, quoted)
	}
	var expected atomic.Value
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		want := expected.Load().(string)
		if r.UserAgent() != want {
			t.Errorf("UA=%q want %q", r.UserAgent(), want)
		}
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "sec-ch-") {
				t.Error("unsolicited hints")
			}
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	for _, name := range []string{"curl", "wget"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatal(err)
		}
		for mode := range 3 {
			want := ua
			var args []string
			if name == "curl" {
				args = []string{"-q", "--config", configs[0], "-sSf"}
			} else {
				args = []string{"--config=" + filepath.Join(filepath.Dir(configs[0]), "wgetrc"), "-qO-"}
			}
			if mode > 0 {
				want = "Explicit/99"
				if mode == 1 {
					args = append(args, "--user-agent", want)
				} else if name == "curl" {
					args = append(args, "--header", "User-Agent: "+want)
				} else {
					args = append(args, "--header=User-Agent: "+want)
				}
			}
			expected.Store(want)
			args = append(args, "--", server.URL+"/?q=$(touch%20ignored)")
			if output, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput(); err != nil {
				t.Fatalf("%s: %v %s", name, err, output)
			}
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("calls=%d", calls)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("shell expression executed")
	}
}

func stringMustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
