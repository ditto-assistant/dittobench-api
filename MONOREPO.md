# Moved to ditto-subnet

Active DittoBench API development, release images, and the hosted Cloud Run
deploy now live in
[`ditto-assistant/ditto-subnet/services/dittobench-api`](https://github.com/ditto-assistant/ditto-subnet/tree/main/services/dittobench-api).

This cutover deliberately removes both legacy automations:

- merges here no longer deploy the hosted API;
- merges here no longer open a cross-repository pull request to repin a remote
  Docker build context in `ditto-subnet`.

The monorepo component graph builds the hosted API and validator-stack scorer
from one immutable release commit. Merge this cutover only after the destination
release and infra PR stacks are ready; this repository remains readable for
history and compatibility-source provenance.
