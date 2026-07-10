#!/usr/bin/env bash
# Build and install the native libraries hap needs since the semantic
# signature matcher landed:
#
#   - libbinding.a + libllama/libggml* from third_party/llama-go (llama.cpp
#     embeddings, statically linked binding + shared runtime libs)
#   - libfaiss_c + libfaiss from the blevesearch FAISS fork (bleve vector
#     search behind the `vectors` build tag)
#
# After this script succeeds, build and test with:
#   go build -tags "vectors cpu" ./...
#   go test  -tags "vectors cpu" ./... -count=1
#
# Idempotent: finished artifacts are skipped, so re-runs are cheap. Set
# HAP_NATIVE_CACHE to relocate the FAISS checkout (default .cache/native).
set -euo pipefail

cd "$(dirname "$0")/.."
REPO_ROOT="$(pwd)"

# blevesearch/faiss checkpoint paired with bleve v2.6.0 (docs/vectors.md).
FAISS_REPO="https://github.com/blevesearch/faiss.git"
FAISS_COMMIT="fff814dea2bdda020363506904979b204ee201aa"
CACHE="${HAP_NATIVE_CACHE:-${REPO_ROOT}/.cache/native}"
PREFIX="${HAP_NATIVE_PREFIX:-/usr/local}"
JOBS="${HAP_NATIVE_JOBS:-$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 2)}"

SUDO="sudo"
if [ -w "${PREFIX}/lib" ] || [ "$(id -u)" = "0" ]; then SUDO=""; fi

case "$(uname -s)" in
  Darwin) SOEXT="dylib" ;;
  *) SOEXT="so" ;;
esac

echo "==> submodules (llama-go + shallow llama.cpp)"
git submodule update --init third_party/llama-go
git -C third_party/llama-go submodule update --init --depth 1 llama.cpp

echo "==> llama-go static binding (BUILD_TYPE=cpu default)"
if [ ! -f third_party/llama-go/libbinding.a ]; then
  make -C third_party/llama-go libbinding.a -j"${JOBS}"
fi

echo "==> FAISS (blevesearch fork @ ${FAISS_COMMIT:0:7})"
mkdir -p "${CACHE}"
if [ ! -d "${CACHE}/faiss" ]; then
  git clone --quiet "${FAISS_REPO}" "${CACHE}/faiss"
fi
git -C "${CACHE}/faiss" checkout --quiet "${FAISS_COMMIT}"
if [ ! -f "${CACHE}/faiss/build/c_api/libfaiss_c.${SOEXT}" ]; then
  cmake -S "${CACHE}/faiss" -B "${CACHE}/faiss/build" \
    -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_SHARED_LIBS=ON \
    -DFAISS_ENABLE_GPU=OFF \
    -DFAISS_ENABLE_PYTHON=OFF \
    -DFAISS_ENABLE_C_API=ON \
    -DFAISS_OPT_LEVEL=generic \
    -DBUILD_TESTING=OFF \
    -DCMAKE_INSTALL_RPATH='$ORIGIN' \
    -DCMAKE_CXX_FLAGS="-I${CACHE}/faiss"
  make -C "${CACHE}/faiss/build" -j"${JOBS}" faiss
  make -C "${CACHE}/faiss/build" faiss_c
fi

echo "==> install shared libraries to ${PREFIX}/lib"
$SUDO mkdir -p "${PREFIX}/lib" "${PREFIX}/include/faiss"
$SUDO cp "${CACHE}/faiss/build/faiss/libfaiss.${SOEXT}" \
  "${CACHE}/faiss/build/c_api/libfaiss_c.${SOEXT}" "${PREFIX}/lib/"
# go-faiss includes <faiss/c_api/...> headers at build time.
$SUDO cp -r "${CACHE}/faiss/c_api" "${PREFIX}/include/faiss/"
# llama-go's binding is static but its ggml/llama runtime libs are shared.
for lib in third_party/llama-go/libllama.${SOEXT} third_party/llama-go/libggml*.${SOEXT}; do
  [ -e "$lib" ] || continue
  $SUDO cp "$lib" "${PREFIX}/lib/"
  base="$(basename "$lib" ".${SOEXT}")"
  # The linker records versioned sonames (libllama.so.0); satisfy them.
  $SUDO ln -sf "${base}.${SOEXT}" "${PREFIX}/lib/${base}.${SOEXT}.0"
done
if command -v ldconfig >/dev/null 2>&1; then $SUDO ldconfig; fi

echo "==> done. Build with: go build -tags \"vectors cpu\" ./..."
