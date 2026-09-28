FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /shadowchat .

FROM scratch
COPY --from=build /shadowchat /shadowchat
USER 65532:65532
EXPOSE 9000
ENTRYPOINT ["/shadowchat"]
