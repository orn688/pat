// Copyright 2022 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// disfunc disassemble a function.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/mgutz/ansi"
)

type disamLine struct {
	file      string
	srcLine   int
	binOffset int
	asm       string
	decoded   string
	instr     string
	arg       string
	alias     string
}

type disasmSym struct {
	file    string
	symbol  string
	content []*disamLine
}

func getDisasm(pkg, bin, filter, file string) ([]*disasmSym, error) {
	if err := exec.Command("go", "build", "-o", bin, pkg).Run(); err != nil {
		return nil, err
	}

	args := []string{"tool", "objdump"}
	if filter != "" {
		args = append(args, "-s", filter)
	}
	args = append(args, bin)
	disasmOut, err := exec.Command("go", args...).Output()
	if err != nil {
		return nil, err
	}

	var out []*disasmSym
	const textPrefix = "TEXT "
	m := map[int]string{}
	for _, l := range strings.Split(string(disasmOut), "\n") {
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, textPrefix) {
			// TEXT github.com/maruel/nin.CanonicalizePath(SB) /home/maruel/src/nin/util.go
			f := strings.SplitN(l[len(textPrefix):], " ", 2)
			if len(f) != 2 {
				return nil, fmt.Errorf("error decoding %q", l)
			}
			d := &disasmSym{
				file:   f[1],
				symbol: f[0],
			}
			out = append(out, d)
			continue
		}
		if !strings.HasPrefix(l, "  ") || len(out) == 0 {
			return nil, fmt.Errorf("error decoding %q", l)
		}
		d := out[len(out)-1]
		// util.go:65            0x505dc0                4c8da42420feffff        LEAQ 0xfffffe20(SP), R12
		l = l[2:]
		i := strings.IndexByte(l, ':')
		j := strings.IndexByte(l, '\t')
		f := l[:i]
		fileSrc := l[:j]
		srcLine, err := strconv.Atoi(l[i+1 : j])
		if err != nil {
			return nil, err
		}
		l = strings.TrimSpace(l[j:])
		j = strings.IndexByte(l, '\t')
		binOffset, err := strconv.ParseInt(l[:j], 0, 0)
		if err != nil {
			return nil, err
		}
		l = strings.TrimSpace(l[j:])
		j = strings.IndexByte(l, '\t')
		asm := l[:j]
		decoded := strings.TrimSpace(l[j:])
		instr := decoded
		arg := ""
		if j = strings.IndexByte(decoded, ' '); j != -1 {
			instr = decoded[:j]
			arg = decoded[j+1:]
		}
		a := &disamLine{
			file:      f,
			srcLine:   srcLine,
			binOffset: int(binOffset),
			asm:       asm,
			decoded:   decoded,
			instr:     instr,
			arg:       arg,
		}
		d.content = append(d.content, a)
		m[int(binOffset)] = fileSrc
	}

	// After parsing everything, resolve the address of the jumps. Do this before
	// filtering just in case.
	for _, s := range out {
		for _, c := range s.content {
			if c.instr[0] == 'J' {
				b, err := strconv.ParseInt(c.arg, 0, 0)
				if err == nil {
					if dst := m[int(b)]; dst != "" {
						c.alias = dst
					}
				}
			}
		}
	}

	if file != "" {
		// Trim out files after the fact. Do it inline if it is observed to be
		// performance critical.
		for i := 0; i < len(out); i++ {
			if filepath.Base(out[i].file) != file {
				copy(out[i:], out[i+1:])
				i--
			}
		}
	}
	return out, nil
}

func printAnnotated(w io.Writer, d []*disasmSym) {
	sort.Slice(d, func(i, j int) bool {
		x := d[i]
		y := d[j]
		if x.file != y.file {
			return x.file < y.file
		}
		return x.symbol < y.symbol
	})
	for _, s := range d {
		d, err := ioutil.ReadFile(s.file)
		if err != nil {
			fmt.Fprintf(w, "couldn't read %q, skipping\n", s.file)
			continue
		}
		lines := strings.Split(string(d), "\n")
		fmt.Fprintf(w, "%s%s%s\n", ansi.LightYellow, s.symbol, ansi.Reset)

		// Reorder by line numbers to make it more easy to understand.
		sort.Slice(s.content, func(i, j int) bool {
			return s.content[i].srcLine < s.content[j].srcLine
		})

		lastLine := 0
		for _, c := range s.content {
			if c.srcLine != lastLine {
				// Print the source line.
				fmt.Fprintf(w, "%d  %s%s%s\n", c.srcLine, ansi.ColorCode("yellow+h+b"), shorten(lines[c.srcLine-1]), ansi.Reset)
				lastLine = c.srcLine
			}

			// Process the decoded line.
			// Colors:
			// - Green:  calls/returns
			// - Red:    traps (UD2)
			// - Blue:   jumps (both conditional and unconditional)
			// - Violet: padding/nops
			// - Yellow: function name
			color := ""
			if c.instr == "CALL" || c.instr == "RET" {
				color = ansi.LightGreen
			} else if strings.HasPrefix(c.instr, "J") {
				color = ansi.LightBlue
			} else if c.instr == "UD2" {
				color = ansi.LightRed
			} else if c.instr == "INT" || strings.HasPrefix(c.instr, "NOP") {
				// Technically it should be INT 3
				color = ansi.LightMagenta
			}
			if c.alias != "" {
				fmt.Fprintf(w, "  %s%-5s %s%s\n", color, c.instr, c.alias, ansi.Reset)
			} else if c.arg != "" {
				fmt.Fprintf(w, "  %s%-5s %s%s\n", color, c.instr, c.arg, ansi.Reset)
			} else {
				fmt.Fprintf(w, "  %s%s%s\n", color, c.instr, ansi.Reset)
			}

			// It's very ISA specific, only tested on x64 for now.
			// Inserts an empty line after unconditional control-flow modifying instructions (JMP, RET, UD2)
			if strings.HasPrefix(c.decoded, "JMP ") || strings.HasPrefix(c.decoded, "RET ") || strings.HasPrefix(c.decoded, "UD2 ") {
				fmt.Fprint(w, "\n")
			}
		}
	}
}

func shorten(l string) string {
	return strings.ReplaceAll(l, "\t", "  ")
}

func mainImpl() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	pkg := flag.String("pkg", ".", "package to build, preferably an executable")
	bin := flag.String("bin", filepath.Base(wd), "binary to generate")
	filter := flag.String("f", "", "function to print out")
	//raw := flag.Bool("raw", false, "raw output")
	//terse := flag.Bool("terse", false, "terse output")
	file := flag.String("file", "", "filter on one file")
	flag.Usage = func() {
		fmt.Printf("usage: disfunc <flags>\n")
		fmt.Printf("\n")
		fmt.Printf("disfunc prints out an annotated function.\n")
		fmt.Printf("It is recommented to use one of -f or -file.\n")
		fmt.Printf("\n")
		fmt.Printf("example:\n")
		fmt.Printf("  disfunc -f 'nin\\.CanonicalizePath$' -pkg ./cmd/nin\n")
		fmt.Printf("\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	s, err := getDisasm(*pkg, *bin, *filter, *file)
	if err != nil {
		return err
	}

	/*
		if *raw {
			printRaw(locs, *file)
			return nil
		}

		if *terse {
			printTerse(locs, *file)
			return nil
		}
	*/
	var w io.Writer = os.Stdout
	if isatty.IsTerminal(os.Stdout.Fd()) && os.Getenv("TERM") != "dumb" {
		w = colorable.NewColorableStdout()
	}
	printAnnotated(w, s)
	return nil
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "disfunc: %s\n", err)
		os.Exit(1)
	}
}
