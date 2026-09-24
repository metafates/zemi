---
name: zemi-worktrees
description: "Load when the user wants several branches worked at once, or a change tried without touching their checkout: git worktrees under .agents/worktrees."
---

# Parallel work with git worktrees

A worktree is a second checkout of the same repository on its own branch, sharing one
`.git`. Two features can be edited, built and tested at the same time without stashing or
switching branches.

Use one only when the user asks for parallel work, or when a change must be tried
without disturbing the checkout they are looking at. For a single feature, a branch in the
current checkout is simpler and leaves nothing to clean up. Never create one to "be safe".

## Where zemi puts them

```sh
git worktree add -b feat/retry .agents/worktrees/retry
```

`.agents/worktrees/<short name>` at the repository root is the right place:

- it is inside the project, so reads need no permission prompt. The rest of `.agents` is
  project config that sandboxed commands cannot write, but a checkout under `.agents/worktrees` is treated like
  the project: auto mode edits it with no approval and sandboxed commands may write it (its own
  `.git` and `.agents` stay protected). A worktree outside the project is outside the jail:
  every read and write there needs approval, one prompt at a time.
- zemi writes `.agents/worktrees/.gitignore` (`/*`) when a sandboxed command next runs and the
  file is missing, so `git status` stays clean, and `grep`, `glob` and ripgrep skip it, so
  a search does not return every hit twice. Nothing under it is loaded as the outer
  project's config, skills, agents or commands.
- `git worktree add` itself writes `.git`, so it needs approval once.

zemi started inside such a worktree shares the main checkout's trust decision, sessions and
memories.

## Running work in one

The path is just a directory: `cd .agents/worktrees/retry && go test ./...` in one bash call (every
call starts in the project directory). Two rules:

- One branch per worktree: git refuses to check a branch out twice, which keeps two changes apart.
- Commands that write to `.git` are shared: a commit, fetch or rebase in one worktree
  locks the index for the others, so do not run two of them at the same instant. Editing,
  building and testing are independent and safe to overlap.

Long builds and test runs belong in `job_start`, so one worktree's build does not hold up
the other.

A subagent (the `agent` tool) is the usual way to drive one: give it the worktree path in
the prompt and let it stay there. Subagents share this session's permission rules and mode.

## Finishing one

```sh
git -C .agents/worktrees/retry add -A && git -C .agents/worktrees/retry commit -m "add retry"
git merge feat/retry              # or: git rebase, or open a PR from the branch
git worktree remove .agents/worktrees/retry
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
| search results appear twice | the worktree is not under `.agents/worktrees/`, or no sandboxed command has run since it was made, so `.agents/worktrees/.gitignore` is missing |
| the worktree has no `node_modules`, `.env`, build cache | untracked and ignored files are not copied; install or symlink them |
| `git worktree remove` fails | uncommitted changes, or the directory was already deleted (`git worktree prune`) |
| project config not loaded in the worktree | the main checkout is not trusted: a worktree shares its trust decision; `/trust` there |
| submodules missing | run `git submodule update --init` in the new worktree |

## Reporting back

Say which worktree a change landed in and on which branch, and whether it is merged. The
user's own checkout is unchanged until you merge, and work left in a worktree they forgot
about is work lost.
