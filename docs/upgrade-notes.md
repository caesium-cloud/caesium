# Upgrade notes

## Unreleased: dataset paths and operator reads

Dataset REST paths now decode escaped path segments exactly once. A name such
as `warehouse/orders` is the complete dataset name; its slash is not a namespace
separator. The namespace remains a separate value supplied by `--namespace`.

Before this correction, an advance sent by the CLI or UI for a slash-containing
name could create a state row under the escaped literal `warehouse%2Forders`.
After upgrading, the same command targets `warehouse/orders`. If the decoded
identity has no state yet, advancing it creates a separate row. The old row's
watermark, advancement timestamps, and derivation history remain under the old
identity; they are not automatically moved or combined.

Before relying on the new identity, inspect both names and retain the old
history for your audit:

```sh
caesium dataset status 'warehouse%2Forders' --json
caesium dataset status 'warehouse/orders' --json
```

A missing decoded row returns 404. Re-run the upstream producer, or advance the
decoded identity with a watermark verified against the source, to establish its
current state. This creates new evidence; it does not merge the old history.
Use the same explicit namespace for both reads when one is present. Automatic
renaming is deliberately avoided because `%2F` can also be part of an intentional
literal name, and both identities may already contain independent history.

`dataset list`, `dataset holds`, and `dataset metrics` expose `--limit` and
`--offset` and print page totals. Metric pages cover raw observations independently
of the configured clean baseline window. Each sample's `in_baseline` field reports
membership in the returned baseline, so an unviolated sample from a failed,
quarantined, or held run is distinguishable from a baseline sample.
Older clean samples outside the configured baseline window also have
`in_baseline: false`.

`dataset holds` targets the empty namespace by default, matching `status`,
`advance`, and `release`. Use `--all-namespaces` for discovery or `--namespace`
for an exact namespace. A release conflict refreshes the active hold and reports
whether the dataset is held again; it does not automatically acknowledge a newer
hold.
