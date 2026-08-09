param(
    [string]$Image = "guanren/clashrulepilot:latest"
)

$ErrorActionPreference = "Stop"

go test ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go test -race ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

docker buildx inspect clashrulepilot-builder *> $null
if ($LASTEXITCODE -ne 0) {
    docker buildx create --name clashrulepilot-builder --use
} else {
    docker buildx use clashrulepilot-builder
}
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

docker buildx build --platform linux/amd64,linux/arm64 --tag $Image --push .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

docker buildx imagetools inspect $Image
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
