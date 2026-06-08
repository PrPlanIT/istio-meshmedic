# syntax=docker/dockerfile:1
FROM golang:1.26.4 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-s -w \
      -X github.com/PrPlanIT/istio-meshmedic/src/version.Version=${VERSION} \
      -X github.com/PrPlanIT/istio-meshmedic/src/version.Commit=${COMMIT} \
      -X github.com/PrPlanIT/istio-meshmedic/src/version.BuildDate=${BUILD_DATE}" \
    -o /out/meshmedic ./cmd/meshmedic

# Pure client-go binary — no runtime tooling needed.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/meshmedic /usr/local/bin/meshmedic
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/meshmedic"]
