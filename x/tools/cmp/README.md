Tool will help you compare results between scip-go and beats for a golang repository

# Running intructions For SCIP

Download scip-go
```
go install github.com/scip-code/scip-go/cmd/scip-go@latest
```

cd into repository say 
```
cd /Users/admin/ws/golang/gitea
```

execute scip-go on repository
```
scip-go --output index.scip
```

# Running intructions For Beats

In case you have not already executed beats on the repository, you may run the following

```
go run ./cmd/ init --repo=/Users/admin/ws/golang/gitea

```
# Running intructions For Beats vs Scip Comparison Tool

```
go run ./x/tools/cmp --scip=/Users/admin/ws/golang/gitea/index.scip --badger=/Users/admin/ws/golang/gitea --repo=/Users/admin/ws/golang/gitea 2>&1 | tee scip_vs_beats.md

```
The generated scip_vs_beats.md would have the summary details at the end of the section.

Few things to remember:

## Recall — beats coverage
Recall answers: does SCIP confirm the cluster?

For each beats cluster, the tool picks the most common SCIP reference symbols across cluster members and runs a query: "find all functions that reference these symbols." Recall is how many of the beats cluster members appear in that query result.

A recall of 1.00 means every function beats grouped is reachable via those SCIP queries — SCIP agrees the cluster is coherent. A recall of 0.60 means 40% of the beats cluster uses the same structural shape but doesn't share the same reference vocabulary, so SCIP's query missed them. These are the structurally-coherent functions that are invisible to reference-based tools.

## Precision — SCIP focus rate
Precision answers: is beats more focused than SCIP?

SCIP queries are broad by design — they return everything that references a given symbol. Precision measures what fraction of the SCIP query result is actually inside the beats cluster.

Low precision (e.g. 0.04) means SCIP's query matched 141 functions but beats only put 5 of them in the cluster. SCIP sees all functions that call xorm.Sync() as equivalent; beats distinguishes sub-clusters within that group by structural shape.

This is the intended behaviour — precision below 1.0 is beats adding signal, not losing it.

## F1 — overall agreement

F1 is the harmonic mean of precision and recall.

It penalises imbalance — a cluster with recall 1.0 and precision 0.04 scores F1 = 0.07, not 0.52. This prevents gaming the metric by either casting a wide net (high recall, low precision) or being overly restrictive (high precision, low recall).

In the beats validation, F1 ≥ 0.7 is treated as "strong match" — SCIP and beats substantially agree. F1 0.4–0.7 is "partial" — some agreement, some divergence. F1 < 0.4 is labelled "novel/noisy": either beats found structure SCIP cannot reproduce, or the SCIP query was too generic (e.g. fmt.Sprintf) to discriminate.