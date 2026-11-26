package com.acme.order;

public class OrderFacade {
    private final int retries;

    public OrderFacade(int retries) {
        this.retries = retries;
    }
}
