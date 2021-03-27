Usage of golint:
	golint [flags] # runs on package in current directory
	golint [flags] [packages]
	golint [flags] [directories] # where a '/...' suffix includes all sub-directories
	golint [flags] [files] # all must belong to a single package
Flags:
  -min_confidence float
    	minimum confidence of a problem to print it (default 0.8)
  -set_exit_status
    	set exit status to 1 if any issues are found
