BIN := miviz

.PHONY: build run clean docker

build:
	cd backend && go build -o ../$(BIN) .

FILE ?= CIRCLONT14.mid
PORT ?=
ARGS ?=

run: build
	./$(BIN) $(if $(PORT),--port $(PORT),--file $(FILE)) $(ARGS)

clean:
	rm -f $(BIN)

docker:
	docker build -t $(BIN) .
