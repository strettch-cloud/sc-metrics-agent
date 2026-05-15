#!/bin/bash
set -uo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}🛡️  Starting Security Scans...${NC}"

# Tool Discovery
TRIVY_CMD=$(which trivy || ( [ -f "$(go env GOPATH)/bin/trivy" ] && echo "$(go env GOPATH)/bin/trivy" ) || echo "trivy-not-found")
GOVULN_CMD=$(which govulncheck || ( [ -f "$(go env GOPATH)/bin/govulncheck" ] && echo "$(go env GOPATH)/bin/govulncheck" ) || echo "govuln-not-found")

EXIT_CODE=0

# 1. Trivy Scan (SCA)
echo -e "\n${YELLOW}[1/2] Running Trivy SCA scan...${NC}"
if [ "$TRIVY_CMD" == "trivy-not-found" ]; then
    echo -e "${RED}Error: trivy not found.${NC}"
    echo "Please install Trivy: https://aquasecurity.github.io/trivy/latest/getting-started/installation/"
    EXIT_CODE=1
else
    if ! "$TRIVY_CMD" fs --severity CRITICAL,HIGH --exit-code 1 --ignore-unfixed \
        --skip-files ".env" --skip-dirs ".envs" --scanners vuln .; then
        echo -e "${RED}Trivy scan detected vulnerabilities!${NC}"
        EXIT_CODE=1
    else
        echo -e "${GREEN}Trivy scan passed.${NC}"
    fi
fi

# 2. Govulncheck
echo -e "\n${YELLOW}[2/2] Running Govulncheck...${NC}"
if [ "$GOVULN_CMD" == "govuln-not-found" ]; then
    echo -e "${RED}Error: govulncheck not found.${NC}"
    echo "Please install: go install golang.org/x/vuln/cmd/govulncheck@latest"
    EXIT_CODE=1
else
    if ! "$GOVULN_CMD" ./...; then
        echo -e "${RED}Govulncheck detected vulnerabilities!${NC}"
        EXIT_CODE=1
    else
        echo -e "${GREEN}Govulncheck passed.${NC}"
    fi
fi

if [ $EXIT_CODE -ne 0 ]; then
    echo -e "\n${RED}🚨 Security vulnerabilities detected or tools missing!${NC}"
    exit 1
fi

echo -e "\n${GREEN}✨ All security scans passed!${NC}"
exit 0
