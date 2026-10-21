---
name: fixloop
description: "Use when a check, test or build fails and its output names several sites to fix: read the failure once, send every edit as edit calls in one response, then run the check again."
---

# fixloop

A failing check is read once and fixed everywhere it points, in one response. The slow shape - edit, check, edit, check - spends a full round trip on a check that could not have been green, because the other sites the failure named were still broken. Three named sites cost two turns fixed this way and six the slow way.

A worked round. The check says:

```
FAIL test_agg: KeyError 'threshold' (agg.py:40)
FAIL test_cli: unrecognized argument --threshold (cli.py:12)
FAIL test_report: threshold never applied (report.py:22)
```

Next response: every site it named, one edit call each, nothing between them.

    edit agg.py      add the threshold field
    edit cli.py      parse --threshold and pass it down
    edit report.py   drop keys below the threshold

Then one check. Green ends the job. Red hands a shorter failure: read it and run the same round again - it converges, because each round clears every site the checker can still name.

Only sites the failure names: a site with no output has not been read yet, and an edit blind to a file costs the read turn it skipped plus a wrong edit. When the fix needs a search first (who calls this?), the search and the edits are two responses: search, then every site the search found in one response.
