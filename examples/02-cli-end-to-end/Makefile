# 02 drives the iplane CLI binary rather than a service. PROVIDER picks
# the backend for the interactive demo; the recorded RUN.md always uses
# local (no API key, $0) so the trace is reproducible.
PROVIDER     ?= local
DEMO_BIN     := cli-end-to-end
DEMO_FLAGS   := --provider $(PROVIDER)
RECORD_FLAGS := --provider local

include ../common/walkthrough.mk
