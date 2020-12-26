#!/bin/zsh

echo 'go tool ...' > alltooldocs/go_tool.go
go tool 2> alltooldocs/go_tool.go 1>> alltooldocs/go_tool.go

go doc cmd/addr2line > alltooldocs/addr2line.go
go doc cmd/api > alltooldocs/api.go
go doc cmd/asm > alltooldocs/asm.go
go doc cmd/buildid > alltooldocs/buildid.go
go doc cmd/cgo > alltooldocs/cgo.go
go doc cmd/compile > alltooldocs/compile.go
go doc cmd/cover > alltooldocs/cover.go
go doc cmd/dist > alltooldocs/dist.go
go doc cmd/doc > alltooldocs/doc.go
go doc cmd/fix > alltooldocs/fix.go
go doc cmd/link > alltooldocs/link.go
go doc cmd/nm > alltooldocs/nm.go
go doc cmd/objdump > alltooldocs/objdump.go
go doc cmd/oldlink > alltooldocs/oldlink.go
go doc cmd/pack > alltooldocs/pack.go
go doc cmd/pprof > alltooldocs/pprof.go
go doc cmd/test2json > alltooldocs/test2json.go
go doc cmd/trace > alltooldocs/trace.go
go doc cmd/vet > alltooldocs/vet.go

gofmt -h 2> alltooldocs/other/gofmt.go 1>> alltooldocs/other/gofmt.go
godoc -h 2> alltooldocs/other/godoc.go 1>> alltooldocs/other/godoc.go
dlv -h 2> alltooldocs/other/dlv.go 1>> alltooldocs/other/dlv.go
dep -h 2> alltooldocs/other/dep.go 1>> alltooldocs/other/dep.go
goimports -h 2> alltooldocs/other/goimports.go 1>> alltooldocs/other/goimports.go
golint -h 2> alltooldocs/other/golint.go 1>> alltooldocs/other/golint.go

