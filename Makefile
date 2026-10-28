ifneq (,$(wildcard ./.env))
include .env
export
endif

# make migration-create <name> — the name is the second goal
NAME := $(word 2,$(MAKECMDGOALS))
ifneq (,$(NAME))
$(NAME):
	@:
endif
.PHONY: $(NAME)

migrate:
	goose -dir ./db/schema postgres "$(DATABASE_URL)" up

rollback:
	goose -dir ./db/schema postgres "$(DATABASE_URL)" down

status:
	goose -dir ./db/schema postgres "$(DATABASE_URL)" status

migration-create:
	@test -n "$(NAME)" || (echo "usage: make migration-create <name>" && exit 1)
	@file=db/schema/$$(date +%Y%m%d%H%M%S)_$(NAME).sql; \
	printf '%s\n' '-- +goose Up' '' '-- +goose Down' > $$file; \
	echo "created $$file"

build:
	go build ./...

vet:
	go vet ./...

.PHONY: migrate rollback status migration-create build vet drill-currency

drill-currency:
	bash scripts/currency-drills.sh
