# Base images are pinned by digest so a retagged image cannot change the build.
FROM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /paibot-gateway ./cmd/paibot-gateway

# Static binary on distroless: no shell, no package manager, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /paibot-gateway /paibot-gateway
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/paibot-gateway", "serve", "-config", "/etc/paibot-gateway/paibot-gateway.json"]
