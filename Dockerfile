# Base images are pinned by digest so a retagged image cannot change the build.
FROM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /paigate ./cmd/paigate

# Static binary on distroless: no shell, no package manager, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /paigate /paigate
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/paigate", "serve", "-config", "/etc/paigate/paigate.json"]
