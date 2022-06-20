// Copyright 2022 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// ba bench against a base commit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"golang.org/x/perf/benchstat"
)

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func runBench(ctx context.Context, pkg, bench string, benchtime time.Duration, count int) (string, error) {
	args := []string{
		"test",
		"-bench", bench,
		"-benchtime", benchtime.String(),
		"-count", strconv.Itoa(count),
		"-run", "^$",
		"-cpu", "1",
	}
	if pkg != "" {
		args = append(args, pkg)
	}
	fmt.Fprintf(os.Stderr, "go %s\n", strings.Join(args, " "))
	out, err := exec.CommandContext(ctx, "go", args...).CombinedOutput()
	return string(out), err
}

// isPristine makes sure the tree is checked out and pristine, otherwise we
// could loose the checkout.
func isPristine() error {
	diff, err := git("status", "--porcelain")
	if err != nil {
		return err
	}
	if diff != "" {
		return errors.New("the tree is modified, make sure to commit all your changes before running this script")
	}
	return nil
}

func getInfos(against string) (string, int, error) {
	// Verify current and against are different commits.
	sha1Cur, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", 0, err
	}
	sha1Ag, err := git("rev-parse", against)
	if err != nil {
		return "", 0, err
	}
	if sha1Cur == sha1Ag {
		return "", 0, errors.New("specify -against to state against why commit to test, e.g. -against HEAD~1")
	}

	// Make sure we'll be able to check the commit back.
	branch, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", 0, err
	}
	if branch == "HEAD" {
		// We're in detached head. It's fine, just save the head.
		branch = sha1Cur[:16]
	}

	commitsHashes, err := git("log", "--format='%h'", sha1Cur+"..."+sha1Ag)
	if err != nil {
		return "", 0, err
	}
	commits := strings.Count(commitsHashes, "\n") + 1
	return branch, commits, nil
}

func warmBench(ctx context.Context, branch, against, pkg, bench string, benchtime time.Duration) error {
	fmt.Fprintf(os.Stderr, "warming up\n")
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := runBench(ctx, pkg, bench, benchtime, 1); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "git checkout %s\n", against)
	out, err := git("checkout", "-q", against)
	if err == nil {
		_, err = runBench(ctx, pkg, bench, benchtime, 1)
	} else {
		err = errors.New(out)
	}
	fmt.Fprintf(os.Stderr, "git checkout %s\n", branch)
	if out2, err2 := git("checkout", "-q", branch); err2 != nil {
		return errors.New(out2)
	}
	return err
}

// runBenchmarks runs benchmarks and return the go test -bench=. result for
// (old, new) where old is `against` and new is HEAD.
func runBenchmarks(ctx context.Context, against, pkg, bench string, benchtime time.Duration, count, series int, nowarm bool) (string, string, error) {
	if err := isPristine(); err != nil {
		return "", "", err
	}
	branch, commits, err := getInfos(against)
	if err != nil {
		return "", "", err
	}

	// TODO(maruel): Make it smart, where it does series until the numbers
	// becomes stable, and actively ignores the higher values.
	// TODO(maruel): When a benchmark takes more than benchtime*count, reduce its
	// count to 1. We could do this by running -benchtime=1x -json.
	// This is particularly problematic with benchmarks lasting less than 100ns
	// per operation as they fail to be numerically stable and deviate by ~3%.
	if !nowarm {
		if err := warmBench(ctx, branch, against, pkg, bench, benchtime); err != nil {
			return "", "", err
		}
	}

	// Run the benchmarks.
	oldStats := ""
	newStats := ""
	needRevert := false
	fmt.Fprintf(os.Stderr, "%s...%s (%d commits), %s x %d times/batch, batch repeated %d times.\n", branch, against, commits, benchtime, count, series)
	for i := 0; i < series; i++ {
		if ctx.Err() != nil {
			// Don't error out, just quit.
			break
		}
		out := ""
		out, err = runBench(ctx, pkg, bench, benchtime, count)
		if err != nil {
			break
		}
		newStats += out

		fmt.Fprintf(os.Stderr, "git checkout %s\n", against)
		needRevert = true
		if out, err = git("checkout", "-q", against); err != nil {
			err = errors.New(out)
			break
		}
		out, err = runBench(ctx, pkg, bench, benchtime, count)
		if err != nil {
			break
		}
		oldStats += out
		fmt.Fprintf(os.Stderr, "git checkout %s\n", branch)
		if out, err = git("checkout", "-q", branch); err != nil {
			err = errors.New(out)
			break
		}
		needRevert = false
	}
	if needRevert {
		fmt.Fprintf(os.Stderr, "Checking out %s\n", branch)
		out := ""
		if out, err = git("checkout", "-q", branch); err != nil {
			err = errors.New(out)
		}
	}
	return oldStats, newStats, err
}

func printBenchstat(w io.Writer, o, n string) error {
	c := &benchstat.Collection{
		Alpha:      0.05,
		AddGeoMean: false,
		DeltaTest:  benchstat.UTest,
	}
	// benchstat assumes that old must be first!
	if err := c.AddFile("HEAD~1", strings.NewReader(o)); err != nil {
		return err
	}
	if err := c.AddFile("HEAD", strings.NewReader(n)); err != nil {
		return err
	}
	benchstat.FormatText(w, c.Tables())
	return nil
}

func mainImpl() error {
	// Reduce runtime interference. 'ba' is meant to be relatively short running
	// and the amount of data processed is small so GC is unnecessary.
	runtime.LockOSThread()
	debug.SetGCPercent(0)
	pkg := flag.String("pkg", "./...", "package to bench")
	bench := flag.String("bench", ".", "benchmark to run, default to all")
	against := flag.String("against", "origin/main", "commitref to benchmark against")
	benchtime := flag.Duration("benchtime", 100*time.Millisecond, "duration of each benchmark")
	count := flag.Int("count", 2, "count to run per attempt")
	series := flag.Int("series", 3, "series to run the benchmark")
	// TODO(maruel): This does not seem to help.
	nowarm := flag.Bool("nowarm", true, "do not run an extra warmup series")
	// TODO(maruel): This does not seem to help.
	spin := flag.Duration("spin", 0, "spin the CPU before benchmark to trigger turbo CPU speed")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: ba <flags>\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "ba (benches against) run benchmarks on two different commits and\n")
		fmt.Fprintf(os.Stderr, "prints out the result with benchstat.\n")
		fmt.Fprintf(os.Stderr, "\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		cancel()
	}()

	if *spin != 0 {
		_ = spinCPU(os.Stderr, *spin)
	}

	oldStats, newStats, err := runBenchmarks(ctx, *against, *pkg, *bench, *benchtime, *count, *series, *nowarm)
	if err2 := printBenchstat(os.Stdout, oldStats, newStats); err2 != nil {
		return err2
	}
	return err
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "ba: %s\n", err)
		os.Exit(1)
	}
}
