package com.example.calc

object Calc {
    fun add(a: Int, b: Int): Int {
        return a + b
    }

    fun sign(n: Int): String {
        if (n >= 0) {
            return "positive"
        } else {
            return "negative"
        }
    }

    fun neverCalled(x: Int): Int {
        val doubled = x * 2
        return doubled + 1
    }
}
