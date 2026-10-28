# Validate

The owner validates attention points, not code: which parts of the specs and
the architecture changed, which screens, with their screenshots, and the
one-way doors. Put them in front of the owner, then act on what they say.

1. Push the checkpoint, and make sure the proposal body states the net change
   and every one-way door it takes.
2. Read the proposal's reviews with `proposals status` and the comments with
   `proposals comments`, and handle them as show's "Intent first" section
   describes.
3. A change request returns the change to code: run
   `"$PUTNAMI_CLI" agent stage code`, repair, and come back through check.
4. With no change request pending, go on to merge. The owner's approval is not
   required for ready; the merge stays the owner's.
