load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "fsutil",
    srcs = [
        "fsutil.go",
        "fsutil_amd64_unsafe.go",
        "fsutil_arm64_unsafe.go",
        "fsutil_darwin.go",
        "fsutil_unsafe.go",
        "fsutil_unsafe_darwin.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/syserr",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "fsutil_test",
    size = "small",
    srcs = ["fsutil_test.go"],
    library = ":fsutil",
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
