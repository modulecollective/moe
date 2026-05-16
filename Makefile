.PHONY: check-git-boundary
check-git-boundary:
	@hits=$$(grep -RnE 'exec\.Command(Context)?\("git"' \
		--include='*.go' \
		. | grep -v '^\./internal/git/' || true); \
	if [ -n "$$hits" ]; then \
		echo 'Forbidden raw git invocations (route through internal/git or internal/git/gittest):'; \
		echo "$$hits"; \
		exit 1; \
	fi
