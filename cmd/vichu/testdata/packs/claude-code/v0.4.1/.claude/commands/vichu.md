---
description: Start or continue a verified VichuFlow run for a task
---

Use the **vichu-orchestrator** skill to handle this request as a verified
VichuFlow run: classify the intent, pick a workflow (`sdd` for features that
deserve a spec + review, `review` for an implement-then-adversarial-review loop,
`quick` for small changes), and drive the run through the `vichu` kernel's
transactional commands — delegating the coding to native subagents and letting
the kernel verify every transition.

Task: $ARGUMENTS
