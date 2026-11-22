public func add(_ a: Int, _ b: Int) -> Int {
    a + b
}

public func sign(_ n: Int) -> String {
    if n >= 0 {
        return "positive"
    } else {
        return "negative"
    }
}

public func neverCalled(_ x: Int) -> Int {
    let doubled = x * 2
    return doubled + 1
}
