package perm

import "golang.org/x/sys/unix"

// searchDir opens a directory to walk it, not to read it: O_PATH needs no
// read or list right, so a walk passes a parent it may not list.
const searchDir = unix.O_PATH | unix.O_DIRECTORY
