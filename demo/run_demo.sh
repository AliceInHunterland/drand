#!/bin/bash

# Build the demo executable first
echo "Building demo..."
go build -o demo
if [ $? -ne 0 ]; then
    echo "Build failed, exiting..."
    exit 1
fi


for (( i=1; i<=10; i++ )); do
    for (( n=87; n>=3; n-=3 )); do
        go build -o demo && ./demo -nodes="${n}"
    done
done
