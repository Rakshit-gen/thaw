// Package tasks lists the agent workloads the benchmarks run: three
// environments, five tasks each. A task counts as answered only when the
// guest's reply holds the expected text, so a fast wrong answer can't win.
//
// The environments are what agents keep warm between steps:
//
//	py-data  a Python kernel holding a 3M-row pandas frame
//	repo     a Django checkout with git and Python already warmed up
//	node-ts  a TypeScript language service over the compiler's own source
//
// Several tasks per environment let the benchmarks ask whether a plan
// learned on one task helps another on the same snapshot.
package tasks

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Rakshit-gen/thaw/vm"
)

// Task is one request to the guest and what a right answer contains.
type Task struct {
	Name   string
	Env    string
	Req    vm.Request
	Expect string
}

// Env describes a VM shape for an environment.
type Env struct {
	Name   string
	VCPUs  int
	MemMiB int
	About  string
}

// Envs are the benchmark environments.
var Envs = []Env{
	{"py-data", 2, 1024, "Python kernel with a 3M-row pandas DataFrame in memory"},
	{"repo", 2, 1024, "Django checkout, git and Python warmed up"},
	{"node-ts", 2, 1536, "TypeScript language service over src/compiler, program built and checked"},
}

// EnvByName finds an environment.
func EnvByName(name string) (Env, bool) {
	for _, e := range Envs {
		if e.Name == name {
			return e, true
		}
	}
	return Env{}, false
}

func py(name, code, expect string) Task {
	return Task{Name: name, Env: "py-data", Req: vm.Request{Op: "py", Code: code}, Expect: expect}
}

func sh(name string, cmd []string, expect string) Task {
	return Task{Name: name, Env: "repo", Req: vm.Request{Op: "exec", Cmd: cmd, Cwd: "/work/django"}, Expect: expect}
}

func ts(name string, q map[string]any, expect string) Task {
	b, _ := json.Marshal(q)
	return Task{Name: name, Env: "node-ts", Req: vm.Request{Op: "ts", Query: b}, Expect: expect}
}

// All is every task.
var All = []Task{
	py("revenue-by-region", `print("revenue", df.groupby("region", observed=True).revenue.sum().round(2).to_dict())`, "revenue {"),
	py("top-customers", `print("top10", df.customer.value_counts().head(10).index.tolist())`, "top10 ["),
	py("monthly-trend", `print("monthly", df.set_index("ts").revenue.resample("M").sum().round(0).tail(3).tolist())`, "monthly ["),
	py("price-quantiles", `print("q", df.price.quantile([.5, .9, .99]).round(2).tolist())`, "q ["),
	py("big-orders", `print("big", int((df[(df.qty >= 4) & (df.price > 100)].groupby("product").revenue.sum() > 1000).sum()))`, "big "),

	sh("git-status", []string{"git", "status", "--short", "--branch"}, "##"),
	sh("grep-slugify", []string{"git", "grep", "-n", "def slugify"}, "django/utils/text.py"),
	sh("log-stat", []string{"git", "log", "-1", "--shortstat", "--format=%H"}, "changed"),
	sh("unit-tests", []string{"env", "PYTHONPATH=/work/django", "python3", "tests/runtests.py", "utils_tests.test_text", "--verbosity", "1"}, "OK"),
	sh("import-models", []string{"python3", "-c", "import django.db.models, django.forms; print('loaded')"}, "loaded"),

	ts("quickinfo", map[string]any{"op": "quickinfo", "file": "checker.ts", "needle": "createTypeChecker("}, "createTypeChecker"),
	ts("definition", map[string]any{"op": "definition", "file": "program.ts", "needle": "createTypeChecker("}, "checker.ts"),
	ts("references", map[string]any{"op": "references", "file": "parser.ts", "needle": "createSourceFile("}, "references"),
	ts("completions", map[string]any{"op": "completions", "file": "utilities.ts", "needle": "node."}, "kind"),
	ts("diagnostics", map[string]any{"op": "diagnostics", "file": "binder.ts"}, "diagnostics"),
}

// ForEnv returns the tasks of one environment.
func ForEnv(env string) []Task {
	var out []Task
	for _, t := range All {
		if t.Env == env {
			out = append(out, t)
		}
	}
	return out
}

// ByName finds a task.
func ByName(name string) (Task, bool) {
	for _, t := range All {
		if t.Name == name {
			return t, true
		}
	}
	return Task{}, false
}

// Check says whether out is a right answer to t.
func (t Task) Check(out string) error {
	if !strings.Contains(out, t.Expect) {
		s := out
		if len(s) > 200 {
			s = s[:200] + "..."
		}
		return fmt.Errorf("%s: answer %q lacks %q", t.Name, s, t.Expect)
	}
	return nil
}
