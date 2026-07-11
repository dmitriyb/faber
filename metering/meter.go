// Package metering is faber's pluggable budget and usage layer: two hooks the
// executor calls around every step (Estimate before launch, Actual after
// completion), endpoint fidelity tiers that implement those hooks at whatever
// fidelity the endpoint allows, a reactive rate-limit defer floor beneath all
// of them, and an opaque unit-tagged cost vocabulary.
//
// The engine never learns what a token, a currency, or a GPU-second is; it
// learns only how to compare tagged quantities and enforce declared limits.
// With no metering config and no budgets the meter set is empty and every
// admission is admit — the no-op default.
package metering

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Unit tags a cost quantity with the user's own vocabulary ("tokens",
// "usd-cents", "gpu-seconds"). Units are opaque to faber: there is no
// exchange rate anywhere in the engine.
type Unit string

// Cost is an opaque unit-tagged quantity. Amount is in the unit's atomic
// granularity (tokens, cents, GPU-seconds — the user's choice); no floats.
type Cost struct {
	Unit   Unit  `json:"unit"`
	Amount int64 `json:"amount"`
}

// Add sums two costs of the same unit. Cross-unit arithmetic is a programming
// error surfaced as an error value, never a silent coercion.
func (c Cost) Add(o Cost) (Cost, error) {
	if c.Unit != o.Unit {
		return Cost{}, fmt.Errorf("metering: cannot add cost in %q to cost in %q: cross-unit arithmetic", o.Unit, c.Unit)
	}
	return Cost{Unit: c.Unit, Amount: c.Amount + o.Amount}, nil
}

// PromptSource yields a step's fully assembled prompt text (skill prompt plus
// context bundle) so tokenizer-based tiers can bound it. Implementations are
// supplied by the executor; TextPrompt covers in-memory text.
type PromptSource interface {
	Prompt(ctx context.Context) (io.Reader, error)
}

// TextPrompt is a PromptSource over an in-memory string.
type TextPrompt string

// Prompt implements PromptSource.
func (t TextPrompt) Prompt(context.Context) (io.Reader, error) {
	return strings.NewReader(string(t)), nil
}

// Step is the estimate view of a launch-ready step: resolved inputs, template
// identity, and the endpoint class the step's agent talks to.
type Step struct {
	NodeID   string
	Template string
	Endpoint string         // endpoint class from the metering config
	Inputs   map[string]any // resolved input values
	Prompt   PromptSource   // skill + context-bundle reader for tokenizers
}

// Result statuses as they appear in a step's result record.
const (
	StatusOK     = "ok"
	StatusFailed = "failed"
)

// ResultView is the actual view of a completed attempt: the step's result
// record plus the decoded usage sidecar the box deposited (nil if none).
type ResultView struct {
	NodeID  string
	Status  string           // StatusOK | StatusFailed
	Usage   map[string]int64 // decoded usage sidecar; nil if none deposited
	Elapsed time.Duration
}

// Estimate is a meter's pre-launch claim: zero or more unit-tagged upper
// bounds (zero amounts participate in admission) and an optional saturation
// hint for endpoints that know their own replenishment window.
type Estimate struct {
	Costs      []Cost     // unit-tagged upper bounds
	DeferUntil *time.Time // saturation hint (probe tier); nil otherwise
}

// Meter is the pluggable seam between the executor and any notion of cost.
// Estimate runs pre-launch and feeds admission: it returns an admission
// bound, not a prediction — over-claiming is safe, under-claiming defeats the
// budget. Actual runs post-completion (every attempt, any status) and feeds
// accounting: its costs replace the step's held reservation in the ledger.
type Meter interface {
	Estimate(ctx context.Context, s Step) (Estimate, error)
	Actual(ctx context.Context, r ResultView) ([]Cost, error)
}

// UnitReporter is an optional Meter extension declaring which units the meter
// can ever report. The Admitter uses it to warn at startup about budgets no
// configured meter measures and to scope estimate failures to the budgets
// they actually protect. All built-in tiers implement it.
type UnitReporter interface {
	Units() []Unit
}
