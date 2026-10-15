#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* 1 = present owned string; 0 = absent; -1 = allocation failure. */
static int confctl_login_copy(char **out) {
    *out = NULL;
    const char *value = getlogin();
    if (value == NULL) value = getenv("USER");
    if (value == NULL) return 0;
    size_t length = strlen(value) + 1;
    char *copy = malloc(length);
    if (copy == NULL) return -1;
    memcpy(copy, value, length);
    *out = copy;
    return 1;
}
