package model_test

import (
	"testing"
	"time"

	"github.com/anishathalye/porcupine"
	"github.com/caesium-cloud/caesium/test/model"
	"pgregory.net/rapid"
)

// The run-status register is the ONE surface in this plan with a valid
// sequential specification, and so the only one Porcupine is used for.
//
// It is a register holding a run's status, written by whichever node owns the
// run and read concurrently by API clients. Its transitions are constrained:
// running settles into one terminal outcome, and only an explicit retry reopens
// it. Every read must return a value the register held at some instant inside
// the read's own interval — which is exactly what linearizability means and
// exactly what a client is entitled to assume.
//
// What is NOT checked here, deliberately: event delivery (at-least-once, so
// duplicates and reordering are legal and no sequential specification exists),
// progress and recovery bounds (liveness, not safety), and quorum-loss
// outcomes (A1 records an unresolved response as possibly committed, which no
// linearizability checker can represent as a return value). Those live in
// events.go, CheckLiveness and the real-cluster scenarios respectively. Running
// them through a linearizability checker would either reject legal histories or
// silently redefine the guarantee.
type regOp string

const (
	opRead       regOp = "read"
	opTransition regOp = "transition"
)

type regInput struct {
	Op    regOp
	Value model.RunStatus
}

// legalTransition is the register's sequential specification.
func legalTransition(from, to model.RunStatus) bool {
	if from == model.RunRunning {
		return model.IsTerminalRun(to)
	}
	// A terminal run reopens only through an explicit retry.
	return model.IsTerminalRun(from) && to == model.RunRunning
}

func runStatusRegister() porcupine.Model {
	return porcupine.Model{
		Init: func() any { return model.RunRunning },
		Step: func(state, input, output any) (bool, any) {
			s := state.(model.RunStatus)
			in := input.(regInput)
			out := output.(model.RunStatus)
			switch in.Op {
			case opRead:
				return out == s, s
			case opTransition:
				if !legalTransition(s, in.Value) {
					return false, s
				}
				return out == in.Value, in.Value
			default:
				return false, s
			}
		},
		Equal: func(a, b any) bool { return a.(model.RunStatus) == b.(model.RunStatus) },
		DescribeOperation: func(input, output any) string {
			in := input.(regInput)
			if in.Op == opRead {
				return "read() -> " + string(output.(model.RunStatus))
			}
			return "transition(" + string(in.Value) + ") -> " + string(output.(model.RunStatus))
		},
	}
}

// register simulates the real thing well enough to emit a history: writers
// transition it at known instants, readers observe it over intervals.
type register struct {
	// timeline records (instant, value) pairs; the value takes effect at the
	// instant and holds until the next one.
	timeline []struct {
		at    int64
		value model.RunStatus
	}
}

func newRegister() *register {
	r := &register{}
	r.timeline = append(r.timeline, struct {
		at    int64
		value model.RunStatus
	}{at: 0, value: model.RunRunning})
	return r
}

func (r *register) set(at int64, v model.RunStatus) {
	r.timeline = append(r.timeline, struct {
		at    int64
		value model.RunStatus
	}{at: at, value: v})
}

func (r *register) current() model.RunStatus { return r.timeline[len(r.timeline)-1].value }

// valuesDuring returns every value the register held at some instant inside a
// closed interval — the full set of answers a read over that interval may
// legally return.
func (r *register) valuesDuring(from, to int64) []model.RunStatus {
	var out []model.RunStatus
	cur := r.timeline[0].value
	for _, point := range r.timeline {
		if point.at <= from {
			cur = point.value
			continue
		}
		if point.at <= to {
			out = append(out, point.value)
		}
	}
	return append([]model.RunStatus{cur}, out...)
}

// TestRunStatusRegisterIsLinearizable generates concurrent histories that are
// linearizable by construction and checks Porcupine agrees.
func TestRunStatusRegisterIsLinearizable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		history, _ := generateRegisterHistory(t, false)
		if res := porcupine.CheckOperationsTimeout(runStatusRegister(), history, 5*time.Second); res == porcupine.Illegal {
			t.Fatalf("a history that is linearizable by construction was rejected: %+v", history)
		}
	})
}

// TestRunStatusRegisterRejectsStaleRead is the checker's own negative control.
//
// An oracle nobody has seen fail is an oracle nobody has tested. This plants
// the defect the register exists to catch — a read that returns a status the
// run never held at any instant during that read — and requires the checker to
// reject it. Without this, a mis-specified model that accepts everything would
// look exactly like a passing test suite.
func TestRunStatusRegisterRejectsStaleRead(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		history, corrupted := generateRegisterHistory(t, true)
		if !corrupted {
			return
		}
		res := porcupine.CheckOperationsTimeout(runStatusRegister(), history, 5*time.Second)
		if res == porcupine.Ok {
			t.Fatalf("the checker accepted a fabricated read: %+v", history)
		}
	})
}

// generateRegisterHistory builds a concurrent history: a writer performing a
// legal transition sequence, and readers whose intervals may overlap the
// writes. When corrupt is set, one read is rewritten to a value that was not in
// effect at any instant of its interval.
func generateRegisterHistory(t *rapid.T, corrupt bool) ([]porcupine.Operation, bool) {
	reg := newRegister()
	var history []porcupine.Operation

	writes := rapid.IntRange(1, 4).Draw(t, "writes")
	terminals := []model.RunStatus{model.RunSucceeded, model.RunFailed, model.RunCancelled}
	// Every value the register ever holds. A read that returns something
	// outside this set is illegal under EVERY linearization, which is the only
	// safe way to plant a defect: a value that merely was not current at the
	// read's call instant may still be legal, because a concurrent write can be
	// linearized before the read.
	held := map[model.RunStatus]bool{model.RunRunning: true}

	var clock int64
	for i := range writes {
		var next model.RunStatus
		if model.IsTerminalRun(reg.current()) {
			next = model.RunRunning
		} else {
			next = rapid.SampledFrom(terminals).Draw(t, "terminal")
		}
		call := clock
		ret := clock + 2
		reg.set(call+1, next)
		held[next] = true
		history = append(history, porcupine.Operation{
			ClientId: 0,
			Input:    regInput{Op: opTransition, Value: next},
			Call:     call,
			Output:   next,
			Return:   ret,
		})
		clock = ret + 1
		_ = i
	}

	reads := rapid.IntRange(0, 6).Draw(t, "reads")
	corrupted := false
	for i := range reads {
		call := int64(rapid.IntRange(0, int(clock)).Draw(t, "read_call"))
		span := int64(rapid.IntRange(0, 3).Draw(t, "read_span"))
		ret := call + span
		options := reg.valuesDuring(call, ret)
		value := options[rapid.IntRange(0, len(options)-1).Draw(t, "observed")]
		if corrupt && !corrupted && i == reads-1 {
			if fabricated, ok := neverHeld(held); ok {
				value = fabricated
				corrupted = true
			}
		}
		history = append(history, porcupine.Operation{
			ClientId: i + 1,
			Input:    regInput{Op: opRead},
			Call:     call,
			Output:   value,
			Return:   ret,
		})
	}
	return history, corrupted
}

// neverHeld returns a run status the register never held in this history, if
// one exists.
func neverHeld(held map[model.RunStatus]bool) (model.RunStatus, bool) {
	for _, candidate := range []model.RunStatus{
		model.RunSucceeded, model.RunFailed, model.RunCancelled, model.RunRunning,
	} {
		if !held[candidate] {
			return candidate, true
		}
	}
	return "", false
}
