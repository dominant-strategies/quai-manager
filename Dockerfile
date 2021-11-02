# Support setting various labels on the final image
ARG COMMIT=""
ARG VERSION=""
ARG BUILDNUM=""

# Build Geth in a stock Go builder container
FROM golang:1.16-alpine as builder

RUN apk add --no-cache gcc musl-dev linux-headers git

ADD . /quai-manager
RUN cd /quai-manager && go build -o manager manager/main.go

WORKDIR /quai-manager
EXPOSE 8545 8546 8547 8548

# Add some metadata labels to help programatic image consumption
ARG COMMIT=""
ARG VERSION=""
ARG BUILDNUM=""

LABEL commit="$COMMIT" version="$VERSION" buildnum="$BUILDNUM"
