VERSION=v0.1
TAGDESC="Initial"
BUILDTIME?=$$(date +%m-%d-%Y-%H:%M)
VERSIONSTRING=${VERSION}-${BUILDTIME}
GOFMT_FILES?=$$(find . -name '*.go')
export GO111MODULE=on

default: bin

all: fmt bin test

bin:
	go build -o ${GOPATH}/bin/vaultcp -ldflags "-X main.versionString=${VERSIONSTRING}" vaultcp.go
	GOOS=linux GOARCH=amd64 go build -o ${GOPATH}/bin/linux_amd64/vaultcp -ldflags "-X main.versionString=${VERSIONSTRING}" vaultcp.go

test:
	go test github.com/richard-mauri/vaultcp

fmt:
	gofmt -w $(GOFMT_FILES)

release:
	git tag -a ${VERSION} -m ${TAGDESC}
	RELVERSION=${VERSIONSTRING} goreleaser 

.PHONY: all bin default test fmt release
