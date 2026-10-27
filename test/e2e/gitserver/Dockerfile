FROM golang:1.27.3-alpine3.24 AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/gitserver .

FROM alpine:3.24
RUN apk add --no-cache git git-daemon \
 && test -x /usr/libexec/git-core/git-http-backend \
 && adduser -D -H -u 10001 git \
 && mkdir -p /srv/git \
 && chown git:git /srv/git
COPY --from=build /out/gitserver /usr/local/bin/gitserver
USER git
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gitserver"]
