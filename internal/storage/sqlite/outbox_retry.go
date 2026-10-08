package sqlite

// RetryExhaustedErrorClass is the error_class recorded on an outbox intent
// that the dispatcher rejected after its transport retry budget ran out. The
// intent was never dispatched; it is terminal (state rejected) and may only be
// sent again explicitly.
const RetryExhaustedErrorClass = "retry_exhausted"
