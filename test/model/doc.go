// Package model is an independent reference model of the Caesium run
// lifecycle.
//
// It exists so the scheduler's decision logic can be differentially tested: the
// model re-derives the same answers from a deliberately different formulation,
// and a disagreement means one of the two is wrong. It is therefore NOT a copy
// of the product algorithm. Where internal/run advances a DAG incrementally
// (predecessor counters decremented on each completion, a ready queue mutated
// in place), this model recomputes readiness declaratively as a fixpoint over
// the current outcome set. Two implementations that share a bug are worth
// nothing; two that share only a specification are worth something.
//
// # What this package is not
//
// A passing model property proves a *decision function* agrees with a
// specification on hermetic inputs. It proves nothing about wiring: not that
// the HTTP handler calls it, not that the transaction commits, not that a real
// owner on a real cluster reaches the same state. Distributed-testing Stream B
// (real multi-node faults) and the integration lanes are what establish that.
// Never cite a green run of this package as evidence that a guarantee holds in
// production.
//
// # Hermetic by construction
//
// This package is deliberately untagged pure Go so it runs under
// `just unit-test`. It must never start a cluster or client, open a Docker
// socket, touch the network or the filesystem outside testdata, or import a
// product decision function (that is the whole point — an "independent" model
// that calls internal/run is a tautology). independence_test.go enforces the
// import allowlist mechanically, so this is a test failure rather than a
// convention.
//
// # Contracts modelled
//
// The identities, acknowledgement points and oracles come from the A1 decision
// record in docs/exec-plans/active/distributed-testing.md. Only the RESOLVED
// subset of each entry is modelled; entries A1 marks unresolved (U) are either
// left out or modelled as explicitly permitted nondeterminism, never as a
// promise.
//
//	DT-ADMIT-01     Admit: an acknowledged run id stays readable and keeps its
//	                job identity. Run.Admit / Run.Acknowledged.
//	DT-OWNER-01     One authoritative owner per run; takeover of an EXPIRED
//	                lease strictly increases the generation. Lease.
//	DT-COMPLETE-01  A completion carrying a stale owner generation is refused
//	                and mutates nothing. Run.Complete + Refusal.
//	DT-TERMINAL-01  Within one execution generation a terminal outcome never
//	                regresses, and a repeat delivery replays the first
//	                delivery's durable effect rather than a fresh one.
//	                ACROSS a retry this is deliberately NOT promised: A1 records
//	                run-execution-epoch fencing as unresolved, so Run.Retry
//	                reopens the run and the oracle scopes itself per epoch.
//	DT-RETRY-01     Retry resets failed work and retains succeeded work.
//	DT-DAG-01       Frozen instance identity, explicit edges, whole-predecessor-
//	                group fan-in, trigger-rule evaluation. DAG + Run.resolve.
//	DT-CANCEL-01    Cancellation resolves work that has not started; work that
//	                has started is left to reach its own terminal state, because
//	                the product cannot kill a running container.
//	DT-EVENT-01     At-least-once delivery with duplicates and out-of-order
//	                live delivery permitted; the persisted sequence is dense
//	                within one store lifetime. Modelled separately in events.go
//	                because delivery has no sequential specification and must
//	                not be checked as one.
//	DT-RECOVER-01   Checkpoint + post-checkpoint terminal tail reconstructs the
//	                same state a crashed owner held, and lost in-flight work is
//	                re-dispatched rather than lost. Run.Checkpoint / Recover.
//
// # Linearizability
//
// Porcupine is used for exactly one thing: the run-status register, which does
// have a valid sequential specification (a monotone state machine read by
// concurrent clients). Liveness, at-least-once event delivery and "possibly
// committed" quorum-loss outcomes have no sequential specification and are
// checked by the separate models here instead. See linearizability_test.go.
//
// # Retained failures
//
// Every counterexample a property finds is minimized and kept. The workflow has
// two halves, and both matter:
//
//  1. Rapid shrinks the failing case and writes the minimized input to
//     testdata/rapid/<TestName>/<TestName>-*.fail. It globs that directory and
//     replays every file it finds before generating anything new. While a
//     defect is OPEN, commit that file: CI then reproduces the exact case
//     instead of waiting to rediscover it.
//  2. When the fix lands, transcribe the minimized case into regression_test.go
//     as a named deterministic test and delete the .fail file.
//
// The second step is not bookkeeping. A .fail file is an opaque bitstream tied
// to the exact sequence of draws the property made, so the first refactor of a
// generator turns it into a "fail file is no longer valid" log line that runs
// nothing and says nothing — and it never explained what the defect WAS even
// while it worked. regression_test.go holds the same cases in a form that
// survives refactoring and tells the next reader why each one exists. The
// corpus in this package is therefore deliberately source, not artifacts.
package model
