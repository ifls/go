usage: goimports [flags] [path ...]
  -cpuprofile string
    	CPU profile output
  -d	display diffs instead of rewriting files
  -e	report all errors (not just the first 10 on different lines)
  -format-only
    	if true, don't fix imports and only format. In this mode, goimports is effectively gofmt, with the addition that imports are grouped into sections.
  -l	list files whose formatting differs from goimport's
  -local string
    	put imports beginning with this string after 3rd-party packages; comma-separated list
  -memprofile string
    	memory profile output
  -memrate int
    	if > 0, sets runtime.MemProfileRate
  -srcdir dir
    	choose imports as if source code is from dir. When operating on a single file, dir may instead be the complete file name.
  -trace string
    	trace profile output
  -v	verbose logging
  -w	write result to (source) file instead of stdout
