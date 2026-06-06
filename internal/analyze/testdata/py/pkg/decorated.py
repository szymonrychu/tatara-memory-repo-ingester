def somedec(fn):
    return fn

def inner_target():
    return 7

@somedec
def decorated_func():
    return inner_target()
