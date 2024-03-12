// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sarif

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"golang.org/x/vuln/internal/govulncheck"
	"golang.org/x/vuln/internal/osv"
)

// handler for sarif output.
type handler struct {
	w    io.Writer
	cfg  *govulncheck.Config
	osvs map[string]*osv.Entry
	// findings contains same-level findings for an
	// OSV at the most precise level of granularity
	// available. This means, for instance, that if
	// an osv is indeed called, then all findings for
	// the osv will have call stack info.
	findings map[string][]*govulncheck.Finding
}

func NewHandler(w io.Writer) *handler {
	return &handler{
		w:        w,
		osvs:     make(map[string]*osv.Entry),
		findings: make(map[string][]*govulncheck.Finding),
	}
}
func (h *handler) Config(c *govulncheck.Config) error {
	h.cfg = c
	return nil
}

func (h *handler) Progress(p *govulncheck.Progress) error {
	return nil // not needed by sarif
}

func (h *handler) OSV(e *osv.Entry) error {
	h.osvs[e.ID] = e
	return nil
}

// moreSpecific favors a call finding over a non-call
// finding and a package finding over a module finding.
func moreSpecific(f1, f2 *govulncheck.Finding) int {
	if len(f1.Trace) > 1 && len(f2.Trace) > 1 {
		// Both are call stack findings.
		return 0
	}
	if len(f1.Trace) > 1 {
		return -1
	}
	if len(f2.Trace) > 1 {
		return 1
	}

	fr1, fr2 := f1.Trace[0], f2.Trace[0]
	if fr1.Function != "" && fr2.Function == "" {
		return -1
	}
	if fr1.Function == "" && fr2.Function != "" {
		return 1
	}
	if fr1.Package != "" && fr2.Package == "" {
		return -1
	}
	if fr1.Package == "" && fr2.Package != "" {
		return -1
	}
	return 0 // findings always have module info
}

func (h *handler) Finding(f *govulncheck.Finding) error {
	fs := h.findings[f.OSV]
	if len(fs) == 0 {
		fs = []*govulncheck.Finding{f}
	} else {
		if ms := moreSpecific(f, fs[0]); ms == -1 {
			// The new finding is more specific, so we need
			// to erase existing findings and add the new one.
			fs = []*govulncheck.Finding{f}
		} else if ms == 0 {
			// The new finding is equal to an existing one and
			// because of the invariant on h.findings, it is
			// also equal to all existing ones.
			fs = append(fs, f)
		}
		// Otherwise, the new finding is at a less precise level.
	}
	h.findings[f.OSV] = fs
	return nil
}

// Flush is used to print out to w the sarif json output.
// This is needed as sarif is not streamed.
func (h *handler) Flush() error {
	sLog := toSarif(h)
	s, err := json.MarshalIndent(sLog, "", "  ")
	if err != nil {
		return err
	}
	h.w.Write(s)
	return nil
}

func toSarif(h *handler) Log {
	cfg := h.cfg
	r := Run{
		Tool: Tool{
			Driver: Driver{
				Name:           cfg.ScannerName,
				Version:        cfg.ScannerVersion,
				InformationURI: "https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck",
				Properties:     *cfg,
				Rules:          rules(h),
			},
		},
		Results: results(h),
	}

	return Log{
		Version: "2.1.0",
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Runs:    []Run{r},
	}
}

func rules(h *handler) []Rule {
	var rs []Rule
	for id := range h.findings {
		osv := h.osvs[id]
		// s is either summary if it exists, or details
		// otherwise. Govulncheck text does the same.
		s := osv.Summary
		if s == "" {
			s = osv.Details
		}
		rs = append(rs, Rule{
			ID:               osv.ID,
			ShortDescription: Description{Text: fmt.Sprintf("[%s] %s", osv.ID, s)},
			FullDescription:  Description{Text: s},
			HelpURI:          fmt.Sprintf("https://pkg.go.dev/vuln/%s", osv.ID),
			Help:             Description{Text: osv.Details},
			Properties:       RuleTags{Tags: osv.Aliases},
		})
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	return rs
}

func results(h *handler) []Result {
	var results []Result
	for _, fs := range h.findings {
		res := Result{
			RuleID:  fs[0].OSV,
			Level:   level(fs[0], h.cfg),
			Message: Description{Text: resultMessage(fs, h.cfg)},
			// TODO: add location and code flows
			Stacks: stacks(fs),
		}
		results = append(results, res)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].RuleID < results[j].RuleID }) // for deterministic output
	return results
}

func resultMessage(findings []*govulncheck.Finding, cfg *govulncheck.Config) string {
	// We can infer the findings' level by just looking at the
	// top trace frame of any finding.
	frame := findings[0].Trace[0]
	uniqueElems := make(map[string]bool)
	if frame.Function == "" && frame.Package == "" { // module level findings
		for _, f := range findings {
			uniqueElems[f.Trace[0].Module] = true
		}
	} else { // symbol and package level findings
		for _, f := range findings {
			uniqueElems[f.Trace[0].Package] = true
		}
	}
	var elems []string
	for e := range uniqueElems {
		elems = append(elems, e)
	}
	sort.Strings(elems)

	l := len(elems)
	elemList := list(elems)
	main, addition := "", ""
	const runCallAnalysis = "Run the call-level analysis to understand whether your code actually calls the vulnerabilities."
	switch {
	case frame.Function != "":
		main = fmt.Sprintf("calls vulnerable functions in %d package%s (%s).", l, choose("", "s", l == 1), elemList)
	case frame.Package != "":
		main = fmt.Sprintf("imports %d vulnerable package%s (%s)", l, choose("", "s", l == 1), elemList)
		addition = choose(", but doesn’t appear to call any of the vulnerable symbols.", ". "+runCallAnalysis, cfg.ScanLevel.WantSymbols())
	default:
		main = fmt.Sprintf("depends on %d vulnerable module%s (%s)", l, choose("", "s", l == 1), elemList)
		informational := ", but doesn't appear to " + choose("call", "import", cfg.ScanLevel.WantSymbols()) + " any of the vulnerable symbols."
		addition = choose(informational, ". "+runCallAnalysis, cfg.ScanLevel.WantPackages())
	}

	return fmt.Sprintf("Your code %s%s", main, addition)
}

const (
	errorLevel         = "error"
	warningLevel       = "warning"
	informationalLevel = "note"
)

func level(f *govulncheck.Finding, cfg *govulncheck.Config) string {
	fr := f.Trace[0]
	switch {
	case cfg.ScanLevel.WantSymbols():
		if fr.Function != "" {
			return errorLevel
		}
		if fr.Package != "" {
			return warningLevel
		}
		return informationalLevel
	case cfg.ScanLevel.WantPackages():
		if fr.Package != "" {
			return errorLevel
		}
		return warningLevel
	default:
		return errorLevel
	}
}

func stacks(fs []*govulncheck.Finding) []Stack {
	if fs[0].Trace[0].Function == "" { // not call level findings
		return nil
	}

	var stacks []Stack
	for _, f := range fs {
		stacks = append(stacks, stack(f))
	}
	// Sort stacks for deterministic output. We sort by message
	// which is effectively sorting by full symbol name. The
	// performance should not be an issue here.
	sort.SliceStable(stacks, func(i, j int) bool { return stacks[i].Message.Text < stacks[j].Message.Text })
	return stacks
}

// stack transforms call stack in f to a sarif stack.
func stack(f *govulncheck.Finding) Stack {
	trace := f.Trace

	var frames []Frame
	for i := len(trace) - 1; i >= 0; i-- { // vulnerable symbol is at the top frame
		frame := trace[i]
		frames = append(frames, Frame{
			Module: frame.Module,
			// TODO: add location
		})
	}

	vuln := trace[0]
	vulnSym := vuln.Function
	if vuln.Receiver != "" {
		vulnSym = vuln.Receiver + "." + vulnSym
	}
	vulnSym = vuln.Package + "." + vulnSym
	return Stack{
		Frames:  frames,
		Message: Description{Text: fmt.Sprintf("A call stack for vulnerable function %s", vulnSym)},
	}
}