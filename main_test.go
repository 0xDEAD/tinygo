package main

// This file tests the compiler by running Go files in testdata/*.go and
// comparing their output with the expected output in testdata/*.txt.

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinygo-org/tinygo/builder"
	"github.com/tinygo-org/tinygo/compileopts"
	"github.com/tinygo-org/tinygo/goenv"
)

const TESTDATA = "testdata"

var testTarget = flag.String("target", "", "override test target")

func TestCompiler(t *testing.T) {
	tests := []string{
		"alias.go",
		"atomic.go",
		"binop.go",
		"calls.go",
		"cgo/",
		"channel.go",
		"float.go",
		"gc.go",
		"goroutines.go",
		"init.go",
		"init_multi.go",
		"interface.go",
		"json.go",
		"map.go",
		"math.go",
		"print.go",
		"reflect.go",
		"slice.go",
		"sort.go",
		"stdlib.go",
		"string.go",
		"structs.go",
		"testing.go",
		"zeroalloc.go",
	}

	_, minor, err := goenv.GetGorootVersion(goenv.Get("GOROOT"))
	if err != nil {
		t.Fatal("could not read version from GOROOT:", err)
	}
	if minor >= 17 {
		tests = append(tests, "go1.17.go")
	}

	if *testTarget != "" {
		// This makes it possible to run one specific test (instead of all),
		// which is especially useful to quickly check whether some changes
		// affect a particular target architecture.
		runPlatTests(*testTarget, tests, t)
		return
	}

	if runtime.GOOS != "windows" {
		t.Run("Host", func(t *testing.T) {
			runPlatTests("", tests, t)
		})
	}

	if testing.Short() {
		return
	}

	t.Run("EmulatedCortexM3", func(t *testing.T) {
		runPlatTests("cortex-m-qemu", tests, t)
	})

	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		// Note: running only on Windows and macOS because Linux (as of 2020)
		// usually has an outdated QEMU version that doesn't support RISC-V yet.
		t.Run("EmulatedRISCV", func(t *testing.T) {
			runPlatTests("riscv-qemu", tests, t)
		})
	}

	if runtime.GOOS == "linux" {
		t.Run("X86Linux", func(t *testing.T) {
			runPlatTests("i386--linux-gnu", tests, t)
		})
		t.Run("ARMLinux", func(t *testing.T) {
			runPlatTests("arm--linux-gnueabihf", tests, t)
		})
		t.Run("ARM64Linux", func(t *testing.T) {
			runPlatTests("aarch64--linux-gnu", tests, t)
		})
		t.Run("WebAssembly", func(t *testing.T) {
			runPlatTests("wasm", tests, t)
		})
		t.Run("WASI", func(t *testing.T) {
			runPlatTests("wasi", tests, t)
		})
	}

	// Test a few build options.
	t.Run("build-options", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			// These tests assume a host that is supported by TinyGo.
			t.Skip("can't test build options on Windows")
		}
		t.Parallel()

		// Test with few optimizations enabled (no inlining, etc).
		t.Run("opt=1", func(t *testing.T) {
			t.Parallel()
			runTestWithConfig("stdlib.go", "", t, compileopts.Options{
				Opt: "1",
			}, nil, nil)
		})

		// Test with only the bare minimum of optimizations enabled.
		// TODO: fix this for stdlib.go, which currently fails.
		t.Run("opt=0", func(t *testing.T) {
			t.Parallel()
			runTestWithConfig("print.go", "", t, compileopts.Options{
				Opt: "0",
			}, nil, nil)
		})

		t.Run("ldflags", func(t *testing.T) {
			t.Parallel()
			runTestWithConfig("ldflags.go", "", t, compileopts.Options{
				GlobalValues: map[string]map[string]string{
					"main": {
						"someGlobal": "foobar",
					},
				},
			}, nil, nil)
		})
	})
}

func runPlatTests(target string, tests []string, t *testing.T) {
	t.Parallel()

	for _, name := range tests {
		name := name // redefine to avoid race condition
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runTest(name, target, t, nil, nil)
		})
	}
	t.Run("env.go", func(t *testing.T) {
		t.Parallel()
		runTest("env.go", target, t, []string{"first", "second"}, []string{"ENV1=VALUE1", "ENV2=VALUE2"})
	})
	if target == "" || target == "wasi" {
		t.Run("filesystem.go", func(t *testing.T) {
			t.Parallel()
			runTest("filesystem.go", target, t, nil, nil)
		})
	}
	if target == "" || target == "wasi" || target == "wasm" {
		t.Run("rand.go", func(t *testing.T) {
			t.Parallel()
			runTest("rand.go", target, t, nil, nil)
		})
	}
}

// Due to some problems with LLD, we cannot run links in parallel, or in parallel with compiles.
// Therefore, we put a lock around builds and run everything else in parallel.
var buildLock sync.Mutex

// runBuild is a thread-safe wrapper around Build.
func runBuild(src, out string, opts *compileopts.Options) error {
	buildLock.Lock()
	defer buildLock.Unlock()

	return Build(src, out, opts)
}

func runTest(name, target string, t *testing.T, cmdArgs, environmentVars []string) {
	options := compileopts.Options{
		Target: target,
	}
	runTestWithConfig(name, target, t, options, cmdArgs, environmentVars)
}

func runTestWithConfig(name, target string, t *testing.T, options compileopts.Options, cmdArgs, environmentVars []string) {
	// Set default config.
	options.Debug = true
	options.VerifyIR = true
	if options.Opt == "" {
		options.Opt = "z"
	}

	// Get the expected output for this test.
	// Note: not using filepath.Join as it strips the path separator at the end
	// of the path.
	path := TESTDATA + "/" + name
	// Get the expected output for this test.
	txtpath := path[:len(path)-3] + ".txt"
	if path[len(path)-1] == '/' {
		txtpath = path + "out.txt"
	}
	expected, err := ioutil.ReadFile(txtpath)
	if err != nil {
		t.Fatal("could not read expected output file:", err)
	}

	// Create a temporary directory for test output files.
	tmpdir := t.TempDir()

	// Determine whether we're on a system that supports environment variables
	// and command line parameters (operating systems, WASI) or not (baremetal,
	// WebAssembly in the browser). If we're on a system without an environment,
	// we need to pass command line arguments and environment variables through
	// global variables (built into the binary directly) instead of the
	// conventional way.
	spec, err := compileopts.LoadTarget(target)
	if err != nil {
		t.Fatal("failed to load target spec:", err)
	}
	needsEnvInVars := spec.GOOS == "js"
	for _, tag := range spec.BuildTags {
		if tag == "baremetal" {
			needsEnvInVars = true
		}
	}
	if needsEnvInVars {
		runtimeGlobals := make(map[string]string)
		if len(cmdArgs) != 0 {
			runtimeGlobals["osArgs"] = strings.Join(cmdArgs, "\x00")
		}
		if len(environmentVars) != 0 {
			runtimeGlobals["osEnv"] = strings.Join(environmentVars, "\x00")
		}
		if len(runtimeGlobals) != 0 {
			// This sets the global variables like they would be set with
			// `-ldflags="-X=runtime.osArgs=first\x00second`.
			// The runtime package has two variables (osArgs and osEnv) that are
			// both strings, from which the parameters and environment variables
			// are read.
			options.GlobalValues = map[string]map[string]string{
				"runtime": runtimeGlobals,
			}
		}
	}

	// Build the test binary.
	binary := filepath.Join(tmpdir, "test")
	err = runBuild("./"+path, binary, &options)
	if err != nil {
		printCompilerError(t.Log, err)
		t.Fail()
		return
	}

	// Create the test command, taking care of emulators etc.
	var cmd *exec.Cmd
	if len(spec.Emulator) == 0 {
		cmd = exec.Command(binary)
	} else {
		args := append(spec.Emulator[1:], binary)
		cmd = exec.Command(spec.Emulator[0], args...)
	}
	if len(spec.Emulator) != 0 && spec.Emulator[0] == "wasmtime" {
		// Allow reading from the current directory.
		cmd.Args = append(cmd.Args, "--dir=.")
		for _, v := range environmentVars {
			cmd.Args = append(cmd.Args, "--env", v)
		}
		cmd.Args = append(cmd.Args, cmdArgs...)
	} else {
		if !needsEnvInVars {
			cmd.Args = append(cmd.Args, cmdArgs...) // works on qemu-aarch64 etc
			cmd.Env = append(cmd.Env, environmentVars...)
		}
	}

	// Run the test.
	runComplete := make(chan struct{})
	ranTooLong := false
	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		t.Fatal("failed to start:", err)
	}
	go func() {
		// Terminate the process if it runs too long.
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-runComplete:
			timer.Stop()
		case <-timer.C:
			ranTooLong = true
			if runtime.GOOS == "windows" {
				cmd.Process.Signal(os.Kill) // Windows doesn't support SIGINT.
			} else {
				cmd.Process.Signal(os.Interrupt)
			}
		}
	}()
	err = cmd.Wait()
	if _, ok := err.(*exec.ExitError); ok && target != "" {
		err = nil // workaround for QEMU
	}
	close(runComplete)

	if ranTooLong {
		stdout.WriteString("--- test ran too long, terminating...\n")
	}

	// putchar() prints CRLF, convert it to LF.
	actual := bytes.Replace(stdout.Bytes(), []byte{'\r', '\n'}, []byte{'\n'}, -1)
	expected = bytes.Replace(expected, []byte{'\r', '\n'}, []byte{'\n'}, -1) // for Windows

	// Check whether the command ran successfully.
	fail := false
	if err != nil {
		t.Log("failed to run:", err)
		fail = true
	} else if !bytes.Equal(expected, actual) {
		t.Log("output did not match")
		fail = true
	}

	if fail {
		r := bufio.NewReader(bytes.NewReader(actual))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			t.Log("stdout:", line[:len(line)-1])
		}
		t.Fail()
	}
}

// This TestMain is necessary because TinyGo may also be invoked to run certain
// LLVM tools in a separate process. Not capturing these invocations would lead
// to recursive tests.
func TestMain(m *testing.M) {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "clang", "ld.lld", "wasm-ld":
			// Invoke a specific tool.
			err := builder.RunTool(os.Args[1], os.Args[2:]...)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	// Run normal tests.
	os.Exit(m.Run())
}
