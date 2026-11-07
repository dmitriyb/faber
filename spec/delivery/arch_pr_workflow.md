# PR workflow — how a change reaches main

Every change lands through a pull request on a branch from `origin/main`, in one
fixed sequence. The sequence is the operator's; the tooling that drives it lives
outside the repository. What the repository requires of it is recorded here so
the history can be read against it.

1. **Proposal.** A change starts as `spec/proposals/YYYY-MM-DD-<slug>.md`
   (Context, Proposed change, Impact expectation), committed on its own.
2. **Spec first.** The spec leaves and `module.json` nodes the change touches are
   edited before any code; the code follows the spec, never the other way round.
   Identity hashes come from `spex hash-id`, never by hand.
3. **Code and tests.** Unit tests are hermetic (no docker, nix or agent CLI);
   whatever needs a daemon goes into the `realinfra`-tagged suite.
4. **Independent review before any commit.** The whole diff, test files
   included, goes to a fresh context with no stake in it, which checks
   correctness and edge cases, spec alignment (the leaf matches the code and sits
   in the right module) and test strength — would the tests fail if the change
   were reverted — and returns findings ranked blocker, should-fix, nit, each
   with a file and line and a failure scenario, and a verdict. Findings are fixed
   in the same change; a large fix is reviewed again.
5. **Real-machine verification** where a daemon is needed: the realinfra suite
   and any probes run on the operator's machines, and their output is seen
   before the implementation commit.
6. **One signed commit per pause**, on the operator's word; the branch is pushed
   on the operator's word; the operator opens the PR with a short title and one
   paragraph (why the change is needed, how it is done), merges it and pulls.

Docs-only changes are never tagged on their own; a release tag goes on the
last PR of a train (`arch_release.md`).
