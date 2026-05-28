.PHONY: build run clean

build:
	go build -o ccg .
	codesign -s - ccg

run: build
	sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add $(CURDIR)/ccg > /dev/null 2>&1 || true
	sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp $(CURDIR)/ccg > /dev/null 2>&1 || true
	set -a && . ./.env && set +a && ./ccg

clean:
	rm -f ccg gateway feishu_state.json
