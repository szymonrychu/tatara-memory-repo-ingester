// Dynamic call via computed member expression - triggers degraded_by
function dynamicCaller(obj, method) { return obj[method](); }
