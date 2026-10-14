package perm

// searchDir opens a directory to walk it, not to read it: O_EXEC|O_DIRECTORY,
// the SDK's O_SEARCH, which x/sys lacks. A walk passes a parent it may not
// list.
const searchDir = 0x40000000 | 0x100000
