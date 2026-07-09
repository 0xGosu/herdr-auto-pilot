#!/bin/bash

# set up env files from default file
[ -f ".env" ] || cp default.env .env

# TODO: add more setup steps here

echo "This script sets up all system config/files required to run the project"