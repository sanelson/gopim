#!/usr/bin/env bash

package=$1
if [[ -z "$package" ]]; then
  echo "usage: $0 <package-name> <platform-name>"
  exit 1
fi
platform_name=$2
package_split=(${package//\// })
package_name=${package_split[-1]}
	
platforms=("windows/amd64" "windows/386" "darwin/amd64" "linux/amd64")

# Default platform is current platform
# Otherwise, build for all platforms if "all" is specified
if [[ -z "$platform_name" ]]; then
  platform_name="$(go env GOOS)/$(go env GOARCH)"
elif [[ "$platform_name" == "darwin/amd64" ]]; then
  if [[ $(go env GOOS) != "darwin" ]]; then
    echo "Cross-compiling for darwin/amd64 is no longer supported"
	exit 1
  fi
fi

for platform in "${platforms[@]}"
do
	# Check if platform is specified
	if [[ "$platform_name" != "all" && "$platform" != "$platform_name" ]]; then
		continue
	fi
	platform_split=(${platform//\// })
	GOOS=${platform_split[0]}
	GOARCH=${platform_split[1]}
	CGO_ENABLED=0
	output_name=$package_name'-'$GOOS'-'$GOARCH
	if [ $GOOS = "windows" ]; then
		output_name+='.exe'
	elif [ $GOOS = "darwin" ]; then
		# Enable CGO for darwin/amd64 and only try to compile all platforms if on OSX
  		if [[ $(go env GOOS) != "darwin" ]]; then
			continue
		fi
		CGO_ENABLED=1
	fi

	env CGO_ENABLED=$CGO_ENABLED GOOS=$GOOS GOARCH=$GOARCH go build \
	  -ldflags="-X main.version=$(git describe --tags)" \
	  -o $output_name $package
	if [ $? -ne 0 ]; then
   		echo 'An error has occurred! Aborting the script execution...'
		exit 1
	fi
done
