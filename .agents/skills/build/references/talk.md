# Talk

Talk turns a problem into a solution the owner accepts, before anything is
declared. The owner may open a session with no task: describe the problem
back, read the code and declarations it touches, and iterate until a viable
solution exists.

1. Orient with the CLI before you propose: `"$PUTNAMI_CLI" context map` for
   the workspace, `context pack` for agent context, `projects describe` for a
   project, `specs` and `features` for its declared requirements.
2. Propose the smallest solution that reaches the outcome, with the
   alternative you rejected and why. A small diagram beats paragraphs for an
   architecture change.
3. Record what the owner settles: it counts as approved at declare.
4. A session may stop here, with no task and no change.

On `--resume`, or when the branch, a proposal or the mission already holds the
work, read the mission and its checkpoint first, then continue at the recorded
stage. Reuse a plan that is still valid after checking the current native
contracts; do not start a new plan because build was invoked again.
