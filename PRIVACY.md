# Privacy Policy — beats

**Last updated: June 2026**

---

## What beats is

beats is a command-line tool and Claude plugin for structural analysis of Go
codebases. It runs locally on your machine. It has no account system, no
analytics, and no servers of its own.

---

## What data beats reads

beats reads Go source files from the repository path you provide. It does not
read files outside that path.

---

## Where beats stores data locally

beats creates two locations on your machine:

**`~/.beats/badger/<repo-name>/`**
A BadgerDB database in your home directory, keyed by repository name. This
stores the structural index — function token sequences, cluster membership,
and orphan candidates — built during `beats init`. It is local-only and never
transmitted anywhere. You can delete it at any time; `beats init` will rebuild
it on the next run.

**`<repo>/.beats/`**
A directory created inside the repository root containing:
- `report.html` — an interactive structural report, local-only
- `outlier.md` — a pre-computed document listing every structural outlier with
  its closest cluster's peer function bodies, used by the Claude skill for
  triage

Both locations contain only data derived from your own source code and are
never transmitted by beats itself.

---

## What gets sent to Anthropic

The beats Claude plugin (the `beats-analyze` skill) sends data to Anthropic's
API in one step: the LLM triage. When you run `mini fingerprint` or `run beats
on <repo>`, the contents of `<repo>/.beats/outlier.md` — which includes
function source code from your repository — are sent to Anthropic's API for
analysis.

This applies to both full mode and mini mode. The only difference is timing:
full mode triggers this after `beats init` completes; mini mode triggers it
directly from an existing `outlier.md`.

Data sent to Anthropic is handled under
[Anthropic's privacy policy](https://www.anthropic.com/privacy) and
[API usage policies](https://www.anthropic.com/legal/aup). beats has no
visibility into or control over how Anthropic handles that data on their side.

---

## What beats does not do

- beats does not collect telemetry, usage data, or crash reports
- beats does not transmit data to any server controlled by this project
- beats does not read environment variables, credentials, or files outside the
  repository path you provide
- beats does not store any data in the cloud

---

## Your responsibility

beats sends source code to a third-party API (Anthropic). You should only run
beats on code you are permitted to share with an external service. If your
repository contains proprietary code, secrets, or data subject to a
confidentiality agreement, it is your responsibility to verify that sharing it
with Anthropic's API is permitted under your applicable agreements.

---