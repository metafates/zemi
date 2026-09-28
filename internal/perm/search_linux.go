package perm

import "golang.org/x/sys/unix"

// searchDir opens a directory to walk it, not to read it: Landlock does not
// check an O_PATH open, so a walk passes a parent the sandbox hides.
const searchDir = unix.O_PATH | unix.O_DIRECTORY
