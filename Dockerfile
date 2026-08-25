# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/voidbar ./cmd/voidbar

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/voidbar /usr/local/bin/voidbar
# /data  - instance storage (users, tokens, networks): mount it
# /mirror - frozen client build from `voidbar mirror`: mount it
VOLUME ["/data", "/mirror"]
ENV VOIDBAR_STORAGE_PATH=/data
EXPOSE 8080
ENTRYPOINT ["voidbar"]
CMD ["serve"]
