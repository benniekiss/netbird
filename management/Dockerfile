FROM golang:alpine3.20 as go-build

RUN apk add --no-cache --update git build-base

RUN go install github.com/goreleaser/goreleaser@latest

COPY . /go/src/netbird

WORKDIR /go/src/netbird

ENV CGO_ENABLED=1
ENV GOOS=linux
ENV GOARCH=amd64

RUN goreleaser build --single-target --snapshot --id netbird-mgmt

FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=go-build \
    /go/src/netbird/dist/netbird-mgmt_linux_amd64_v1/netbird-mgmt \
    /go/bin/

RUN adduser -S netbird

RUN mkdir -p /var/lib/netbird \
    && chown netbird /var/lib/netbird

USER netbird

ENTRYPOINT [ "/go/bin/netbird-mgmt","management"]
CMD ["--log-file", "console"]
