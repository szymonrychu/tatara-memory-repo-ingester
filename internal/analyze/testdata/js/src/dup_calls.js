function target() { return 1; }

function caller() {
    target();
    target();
    target();
}
