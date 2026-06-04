FROM golang:1.20-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git
COPY go.mod ./
COPY main.go ./
RUN go mod tidy
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=mod -o /waha-openai .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /waha-openai /app/waha-openai
EXPOSE 8080
CMD ["/app/waha-openai"]
