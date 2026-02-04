.PHONY: all build clean

all: build

build:
	./build_vendor.sh

clean:
	rm -f bin/goftpfs
	rm -rf bin
