/* A dependency-free fixture for Sliver's native extension callback ABI. */

typedef __UINT32_TYPE__ fixture_u32;
typedef __INT32_TYPE__ fixture_i32;
typedef fixture_i32 (*fixture_callback)(char *, fixture_i32);

#define FIXTURE_EXPORT __attribute__((visibility("default")))

static int fixture_initialized;
static char fixture_result[64];

FIXTURE_EXPORT fixture_i32 FixtureInit(void) {
    fixture_initialized = 1;
    return 0;
}

FIXTURE_EXPORT fixture_i32 FixtureEcho(
    char *input,
    fixture_u32 input_size,
    fixture_callback callback
) {
    static const char prefix[] = "extension-ok:";
    fixture_u32 i;
    const fixture_u32 prefix_size = (fixture_u32)(sizeof(prefix) - 1);

    if (!fixture_initialized || callback == 0 ||
        (input == 0 && input_size != 0) ||
        input_size > sizeof(fixture_result) - prefix_size) {
        return -1;
    }

    for (i = 0; i < prefix_size; i++) {
        fixture_result[i] = prefix[i];
    }
    for (i = 0; i < input_size; i++) {
        fixture_result[prefix_size + i] = input[i];
    }
    return callback(fixture_result, (fixture_i32)(prefix_size + input_size));
}
