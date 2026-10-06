# micro-common 常用命令（S0-02）
# 用法：make <target>；Windows 本地若无 make，按各 target 注释直接执行等价命令
# 注意：S1 前仓库只有 go.mod 无源码，build/test/vet/lint 自动跳过（与 CI 守卫一致）
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

SOURCES := $(shell git ls-files '*.go')

.DEFAULT_GOAL := help
.PHONY: build test vet lint tidy fmt help

## build: 编译全部包（S1 前无源码自动跳过）
build:
	@if [ -z "$(SOURCES)" ]; then echo "S1 前无 Go 源码：跳过"; else go build ./...; fi

## test: 全量单测（S1 前无源码自动跳过）
test:
	@if [ -z "$(SOURCES)" ]; then echo "S1 前无 Go 源码：跳过"; else go test ./...; fi

## vet: go vet ./...（S1 前无源码自动跳过）
vet:
	@if [ -z "$(SOURCES)" ]; then echo "S1 前无 Go 源码：跳过"; else go vet ./...; fi

## lint: golangci-lint v2（CI 同款；S1 前无源码自动跳过）
lint:
	@if [ -z "$(SOURCES)" ]; then echo "S1 前无 Go 源码：跳过"; else golangci-lint run ./...; fi

## tidy: go mod tidy
tidy:
	go mod tidy

## fmt: gofmt 全仓
fmt:
	@if [ -z "$(SOURCES)" ]; then echo "S1 前无 Go 源码：跳过"; else gofmt -l -w .; fi

## help: 目标清单
help:
	@echo "build / test / vet / lint / tidy / fmt —— tag 由 CI 自动打（合并 main，v0.1.x）"
