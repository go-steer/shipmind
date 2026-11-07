# Judge calibration

Hand-written answers to the nightly scenarios, each with a known flaw and
the score band (`want`) the judge's rubric should give it. With the
archived runs, which are correct answers, they check that a judge marks
wrong, vague and half-right answers down, and that it gives the same
grade twice.

Grade them, and archived runs copied down from the bucket, with:

```
shipmind scenario judge -model claude-sonnet-5-5 -project gke-demos-345619 \
  e2e/calibration/*.json runs/*.json
```

Each file has a result.json's `scenario`, `workload` and `answer`, and the
`want` band. Re-run the set before changing the judge model, its effort or
the rubric (docs/decisions.md, 2026-11-07), and add cases when a scenario
is added. The first calibration (2026-11-07): Sonnet 5.5 and Opus 5.5 both
put all 24 answers in band, twice.

| Case | Flaw | Want |
|---|---|---|
| missing-env-wrong-cause | blames an OOM kill | 1-2 |
| missing-env-generic | names nothing, fixes nothing | 1-2 |
| missing-env-wrong-resource | invents a missing Secret | 2-3 |
| missing-env-invented-value | right cause and place, made-up connection string | 3-4 |
| missing-env-no-fix | right and specific, no remediation | 3-4 |
| oom-killed-other-incident | answers a different incident | 1 |
| oom-killed-blames-node | node memory pressure | 1-2 |
| oom-killed-blames-code | a memory leak, not the limit | 2-3 |
| oom-killed-vague | right cause, no resources named | 3-4 |
| oom-killed-wrong-number | right cause and fix, wrong limit value | 3-4 |
