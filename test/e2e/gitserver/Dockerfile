FROM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/gitserver .

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache git git-daemon \
 && test -x /usr/libexec/git-core/git-http-backend \
 && adduser -D -H -u 10001 git \
 && mkdir -p /srv/git \
 && chown git:git /srv/git
COPY --from=build /out/gitserver /usr/local/bin/gitserver
USER git
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gitserver"]
