// Copyright 2020 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"go.skia.org/infra/go/exec"
	"go.skia.org/infra/go/util"
	"go.skia.org/infra/task_driver/go/td"
)

type work struct {
	Sources []string
	Flags   []string
}

func main() {
	var (
		projectId = flag.String("project_id", "", "ID of the Google Cloud project.")
		taskId    = flag.String("task_id", "", "ID of this task.")
		taskName  = flag.String("task_name", "", "Name of the task.")
		output    = flag.String("o", "", "Dump JSON step data to the given file, or stdout if -.")
		local     = flag.Bool("local", true, "Running locally (else on the bots)?")

		resources = flag.String("resources", "resources", "Passed to fm -i.")
	)
	ctx := td.StartRun(projectId, taskId, taskName, output, local)
	defer td.EndRun(ctx)

	actualStderr := os.Stderr
	if *local {
		// Task Driver echoes every exec.Run() stdout and stderr to the console,
		// which makes it hard to find failures (especially stdout).  Send them to /dev/null.
		devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			td.Fatal(ctx, err)
		}
		os.Stdout = devnull
		os.Stderr = devnull
	}

	if flag.NArg() < 1 {
		td.Fatalf(ctx, "Please pass an fm binary.")
	}
	fm := flag.Arg(0)

	// Run `fm <flag>` to find the names of all linked GMs or tests.
	query := func(flag string) []string {
		stdout := &bytes.Buffer{}
		cmd := &exec.Command{Name: fm, Stdout: stdout}
		cmd.Args = append(cmd.Args, "-i", *resources)
		cmd.Args = append(cmd.Args, flag)
		if err := exec.Run(ctx, cmd); err != nil {
			td.Fatal(ctx, err)
		}

		lines := []string{}
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			td.Fatal(ctx, err)
		}
		return lines
	}
	gms := query("--listGMs")
	tests := query("--listTests")

	// Parse a job like "gms b=cpu ct=8888" into a struct of Sources to run under given Flags.
	parse := func(job []string) *work {
		w := &work{}

		for _, token := range job {
			// Everything after # is a comment.
			if strings.HasPrefix(token, "#") {
				break
			}

			// Treat "gm" or "gms" as a shortcut for all known GMs.
			if token == "gm" || token == "gms" {
				w.Sources = append(w.Sources, gms...)
				continue
			}
			// Same for tests.
			if token == "test" || token == "tests" {
				w.Sources = append(w.Sources, tests...)
				continue
			}

			// Is this a flag to pass through to FM?
			if parts := strings.Split(token, "="); len(parts) == 2 {
				f := "-"
				if len(parts[0]) > 1 {
					f += "-"
				}
				f += parts[0]

				w.Flags = append(w.Flags, f, parts[1])
				continue
			}

			// Anything else must be the name of a source for FM to run.
			w.Sources = append(w.Sources, token)
		}

		return w
	}

	// TODO: this doesn't have to be hard coded, of course.
	// TODO: add some .skps or images to demo that.
	script := `
	b=cpu tests
	b=cpu gms
	b=cpu gms skvm=true

	#b=cpu gms skvm=true gamut=p3
	#b=cpu gms skvm=true ct=565
	`
	jobs := [][]string{}
	scanner := bufio.NewScanner(strings.NewReader(script))
	for scanner.Scan() {
		jobs = append(jobs, strings.Fields(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		td.Fatal(ctx, err)
	}

	// We'll kick off workers to run FM with `-s <Sources...> <Flags...>` from parsed jobs.
	var failures int32 = 0
	wg := &sync.WaitGroup{}

	worker := func(queue chan work) {
		for w := range queue {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			cmd := &exec.Command{Name: fm, Stdout: stdout, Stderr: stderr}
			cmd.Args = append(cmd.Args, "-i", *resources)
			cmd.Args = append(cmd.Args, "-s")
			cmd.Args = append(cmd.Args, w.Sources...)
			cmd.Args = append(cmd.Args, w.Flags...)
			if err := exec.Run(ctx, cmd); err != nil {
				// We optimistically run batches of Sources, but if a batch fails,
				// we'll re-run one at a time to find the precise failures.
				if len(w.Sources) == 1 {
					// If a source ran alone and failed, that's just a failure.
					atomic.AddInt32(&failures, 1)
					td.FailStep(ctx, err)
					if *local {
						lines := []string{}
						scanner := bufio.NewScanner(stderr)
						for scanner.Scan() {
							lines = append(lines, scanner.Text())
						}
						if err := scanner.Err(); err != nil {
							td.Fatal(ctx, err)
						}

						fmt.Fprintf(actualStderr, "%v %v #failed:\n\t%v\n",
							cmd.Name,
							strings.Join(cmd.Args, " "),
							strings.Join(lines, "\n\t"))
					}
				} else {
					// If a batch fails, retry each individually.
					for _, source := range w.Sources {
						// Requeuing work from the workers makes sizing the chan buffer tricky:
						// we don't ever want this `queue <-` to block on a full buffer.
						wg.Add(1)
						queue <- work{[]string{source}, w.Flags}
					}
				}
			}
			wg.Done()
		}
	}

	workers := runtime.NumCPU()
	queue := make(chan work, 1<<20) // Huge buffer to avoid having to be smart about requeuing.
	for i := 0; i < workers; i++ {
		go worker(queue)
	}

	for _, job := range jobs {
		w := parse(job)
		if len(w.Sources) == 0 {
			continue // A blank/commented line in the job script.
		}

		// Shuffle the sources randomly as a cheap way to approximate evenly expensive batches.
		// (Intentionally not rand.Seed()'d to stay deterministically reproducible.)
		rand.Shuffle(len(w.Sources), func(i, j int) {
			w.Sources[i], w.Sources[j] = w.Sources[j], w.Sources[i]
		})

		// Round batch sizes up so there's at least one source per batch.
		batch := (len(w.Sources) + workers - 1) / workers
		util.ChunkIter(len(w.Sources), batch, func(start, end int) error {
			wg.Add(1)
			queue <- work{w.Sources[start:end], w.Flags}
			return nil
		})
	}
	wg.Wait()

	if failures > 0 {
		if *local {
			// td.Fatalf() would work fine, but barfs up a panic that we don't need to see.
			fmt.Fprintf(actualStderr, "%v runs of %v failed after retries.\n", failures, fm)
			os.Exit(1)
		} else {
			td.Fatalf(ctx, "%v runs of %v failed after retries.", failures, fm)
		}
	}
}
