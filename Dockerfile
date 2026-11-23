FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY VERSION ./
COPY cmd ./cmd
COPY internal ./internal
RUN version=$(cut -d' ' -f1 VERSION) \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$version" -o /out/annalsd ./cmd/annalsd \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$version" -o /out/annals-m365 ./cmd/annals-m365

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/annalsd /app/annalsd
# The Microsoft 365 collector, run as its own deployment with
# command /app/annals-m365.
COPY --from=build /out/annals-m365 /app/annals-m365
EXPOSE 8080 8081
USER nonroot
ENTRYPOINT ["/app/annalsd"]
