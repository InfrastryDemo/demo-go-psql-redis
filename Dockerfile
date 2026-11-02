FROM golang:1.27-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /room .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /room /room
USER 10001:10001
EXPOSE 8090
ENTRYPOINT ["/room"]
