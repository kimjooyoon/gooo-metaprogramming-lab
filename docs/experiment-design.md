# Experiment design

## Hypothesis

Inference can improve code structure when it makes a bounded decision over
semantic IR, while deterministic compiler rules retain control of the actual
rewrite and source emission. The experiment asks whether Laya-selected
transformations increase requirement-backed structural completeness over an
unchanged IR without reducing validity, compilation, replayability, or
provenance.

This places inference **between IR construction and deterministic projection**.
It does not place inference inside Go syntax emission and does not treat model
confidence as evidence that a candidate is correct.

## Processing stages

1. Read a case containing a task description, baseline IR, bounded
   transformations, and a hidden-from-the-model structural contract.
2. Ask `gooo decide` to choose one transformation ID. The model sees the task
   and short descriptions of the available transformations, but not the
   expected inventory used by the evaluator.
3. Apply the chosen transformation as a deterministic IR rewrite. The same
   baseline IR plus the same transformation ID must always yield identical IR.
4. Validate names and references, render the supported IR subset as `.gooo`,
   then call Gooo's normal `generate` command.
5. Parse the emitted Go, compile it, calculate contract coverage, and compare
   the selected result with both the unchanged baseline and the best compiling
   candidate.
6. Write a versioned report containing input, decision, IR, and generated
   artifact digests plus all metric denominators.

The model only selects a transformation identifier. A transformation is
ordinary deterministic code in this lab. Generated files stay under the
caller-provided output directory; the runner never edits a source repository.

## First case: payment workflow declarations

The fixture starts with `Order`, `PaymentMethod`, and `Payment`, plus one
`PayOrder` signature. The alternative adds named authorization, receipt, and
audit entities and corresponding typed activity signatures. This tests whether
the model can route an intent to a structural pattern that covers a richer
contract.

Gooo's current source language projects entities and activities, but this
fixture does not encode control-flow edges between those activities. The first
case therefore measures declaration and signature completeness only. It must
not be described as proving that payment authorization, receipt creation, or
audit writing actually executes.

## Metric contract

The evaluator uses exact requirements in each case, with a fixed denominator.
It emits the component values separately rather than collapsing them into one
quality score.

| Metric | Numerator / denominator | Role |
| --- | --- | --- |
| Entity coverage | required entity types found in generated Go / required entity types | Structural completeness |
| Activity coverage | required functions found / required functions | Structural completeness |
| Port coverage | required input and output type occurrences found / required port occurrences | Structural completeness |
| Structural completeness | satisfied entity + activity + port atoms / all declared contract atoms | Comparable summary for this case |
| IR validity | valid IR: 1, invalid IR: 0 | Hard gate |
| Go compilation | compiler succeeds: 1, otherwise 0 | Hard gate |
| Generation determinism | identical IR and output digests across replay / replay count | Reproducibility gate |
| Behavioral completeness | `NOT_SCORABLE` until the language has executable behavior and an independent oracle | Explicit limit |

Missing requirements remain visible by ID. A high structural percentage cannot
cancel an IR-invalid or compile-failing result. Behavioral correctness is never
inferred from successful parsing or compilation.

For a corpus-wide claim, aggregate numerators and denominators across cases;
also report per-case values and the worst-case result. Do not average
percentages with different denominators.

## Comparisons and attribution

- **Baseline:** apply no transformation and run the normal Gooo generator.
- **Laya-selected:** apply only the ID returned by `gooo decide`.
- **Oracle upper bound:** evaluate every allowed deterministic transformation
  against the contract and choose the best candidate that passes both hard
  gates. This is not a deployable selection algorithm; it shows how much
  completeness the candidate set makes available.
- **Deterministic fallback:** run with Laya absent and verify that the declared
  fallback yields the same selection and digests on replay.

Report selected-minus-baseline structural completeness, the selected-to-oracle
gap, compile and validity regressions, latency, and the chosen model/checkpoint
revision. Evaluate Laya accuracy and calibration only after collecting a
diverse, independently labeled corpus. Repeating one prompt many times is a
stability observation, not an accuracy sample.

## System-driven improvement loop

1. Expand the fixture corpus from explicit, versioned contracts. Keep each
   contract out of the decision request so evaluation does not leak its answer.
2. Run the current model in shadow mode and save every decision and artifact
   digest.
3. Compare against the deterministic baseline and oracle using frozen cases.
4. Add a new rewrite pattern only when it expresses a supported, deterministic
   IR transformation with a measurable contract effect.
5. Promote a model or routing rule only when structural completeness improves
   across the frozen cohort, hard-gate regressions remain zero, determinism
   holds, and latency/resource costs stay within an explicit budget.
6. Fall back to the declared deterministic choice for missing, unavailable, or
   malformed Laya output. Add a low-confidence fallback only after confidence
   is calibrated on Gooo-specific outcomes.

The current base Laya checkpoints score below the majority baseline on the
model card's typed-decisions benchmark. That makes this an experiment, not an
assumption that zero-shot Laya already chooses good compiler structures. The
model card also reports poor performance for large option sets, so this lab
keeps the choices small and bounded. See the [Laya model card](https://huggingface.co/convaiinnovations/laya).

## Performance budget starting point

The Gooo repository measured 30 warm local CPU-only calls on an Apple M4:
Gooo-to-Laya p50 61.59 ms / p95 63.24 ms, deterministic fallback p50 4.55 ms,
and Laya process CPU averaging 105.21% of one core during the call batch. This
is a machine-specific baseline, not a portable target. The first lab report
must measure its own decision latency and must keep CPU, memory, and generated
artifact size alongside completeness.
