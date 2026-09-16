---
name: zemi-worktrees
description: Work on several branches at once with git worktrees - where to put them, how to keep them out of search and status, how to finish and remove one.
---

# Parallel work with git worktrees

A worktree is a second checkout of the same repository on its own branch, sharing one
`.git`. Two features can be edited, built and tested at the same time without stashing or
switching branches.

**Use one only when the user asks for parallel work**, or when a change must be tried
without disturbing the checkout they are looking at. For a single feature, a branch in the
current checkout is simpler and leaves nothing to clean up. Never create one to "be safe".

## Where zemi puts them

```sh
git worktree add -b feat/retry .worktrees/retry
echo '.worktrees/' >> .git/info/exclude       # once per repository
```

`.worktrees/<short name>` under the directory zemi started in (the Environment section names
it; it is the repo root in the normal case) is the right place, for three reasons:

- it is inside the project, so reads need no permission prompt and writes ask exactly like
  any other project file. A worktree outside that directory is outside the jail: every read
  and write there asks, one prompt at a time. Started in a subdirectory, put `.worktrees/`
  there rather than at the repo root, or the jail does not cover it.
- `.git/info/exclude` is local to the clone, so the entry never shows in a diff or needs
  committing, and it keeps `git status` clean.
- `grep`, `glob` and ripgrep honour that exclude, so a search does not return every hit
  twice, and the Go, Node and Python toolchains all skip a leading-dot directory.

Check the exclude entry is there before the first `git worktree add`; without it every
search in the repository doubles.

## Running work in one

The path is just a directory: `cd .worktrees/retry && go test ./...` in one bash call, or
pass it as `cwd`-style prefix per command. Two rules:

- **One branch per worktree.** git refuses to check the same branch out twice, and that
  refusal is the feature: it is what keeps two parallel changes from fighting.
- **Commands that write to `.git` are shared.** A commit, fetch or rebase in one worktree
  locks the index for the others, so do not run two of them at the same instant. Editing,
  building and testing are independent and safe to overlap.

Long builds and test runs belong in `zemi.job` or a background bash call, so one worktree's
build does not hold up the other.

A subagent (the `agent` tool) is the usual way to drive one: give it the worktree path in
the prompt and let it stay there. Subagents share this session's permission rules, so an
allow answered in the main checkout covers the worktree too.

## Finishing one

```sh
cd .worktrees/retry && git add -A && git commit -m "add retry" && cd -
git merge feat/retry              # or: git rebase, or open a PR from the branch
git worktree remove .worktrees/retry
git branch -d feat/retry          # only after it is merged
```

`git worktree remove` refuses while the tree is dirty; commit or discard first, and pass
`--force` only when the user says the work is disposable. Deleting the directory by hand
leaves a stale entry behind - `git worktree prune` clears those.

`git worktree list` shows every checkout and the branch it holds; run it before creating one
so an abandoned worktree is reused instead of piling up.

## Pitfalls

| Symptom | Cause |
| --- | --- |
| `fatal: '<branch>' is already checked out` | another worktree holds it; `git worktree list` says which |
| search results appear twice | `.worktrees/` is missing from `.git/info/exclude` |
| the worktree has no `node_modules`, `.env`, build cache | untracked and ignored files are not copied; install or symlink them |
| `git worktree remove` fails | uncommitted changes, or the directory was already deleted (`git worktree prune`) |
| project config not loaded in the worktree | `.zemi/` is a separate path there, so zemi asks to trust it again |
| submodules missing | run `git submodule update --init` in the new worktree |

## Reporting back

Say which worktree a change landed in and on which branch, and whether it is merged. The
user's own checkout is unchanged until you merge, and work left in a worktree they forgot
about is work lost.
