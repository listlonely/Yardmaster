# yardmaster 开发命令（开发文档 §10.C）
# Windows 下若未安装 make，直接运行等价的 go 命令即可；
# 本机 Go 不在 PATH 时：set GO=C:\Users\<you>\tools\go\bin\go.exe

GO ?= go

.PHONY: lint test vet build clean

lint:
	$(GO) vet ./...
	-golangci-lint run ./...

test:
# 说明：Windows 本地 -race 需要 gcc（CGO_ENABLED=1），无 gcc 时请直接
# 运行 go test ./...；-race 在 CI 双平台强制执行（开发文档 §5.2）。
	$(GO) test -race -cover ./...

vet:
	$(GO) vet ./...
	GOOS=windows $(GO) vet ./...
	GOOS=linux $(GO) vet ./...

build:
	$(GO) build -trimpath -o bin/ ./cmd/yardmaster

clean:
	-$(RM) -r bin
