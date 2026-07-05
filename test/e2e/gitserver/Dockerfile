# Throwaway smart-HTTP git server for the U1 e2e suite only (never shipped).
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/gitserver ./test/e2e/gitserver

FROM alpine:3.20
# git provides git-http-backend at /usr/libexec/git-core/.
RUN apk add --no-cache git
COPY --from=build /out/gitserver /usr/local/bin/gitserver
RUN mkdir -p /srv/git && adduser -D -u 65532 kohen && chown -R kohen /srv/git
USER 65532
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/gitserver", "-addr", ":8443", "-root", "/srv/git"]
