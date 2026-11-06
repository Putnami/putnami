package com.example.calc;

public final class Calc {
    private Calc() {
    }

    public static int add(int a, int b) {
        return a + b;
    }

    public static String sign(int n) {
        if (n >= 0) {
            return "positive";
        } else {
            return "negative";
        }
    }

    public static int neverCalled(int x) {
        int doubled = x * 2;
        return doubled + 1;
    }
}
