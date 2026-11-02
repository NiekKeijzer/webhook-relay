# One image for both components:
#
#	docker run <image> receiver [-config path] [-listen addr]
#	docker run <image> forwarder [-config path]
#
# The binary is cross-compiled on the build platform, so building for another
# architecture (--platform) needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /relay /relay
EXPOSE 8080
ENTRYPOINT ["/relay"]
