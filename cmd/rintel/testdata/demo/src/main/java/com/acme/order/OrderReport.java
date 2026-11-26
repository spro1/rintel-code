package com.acme.order;

import com.acme.user.User;

public class OrderReport {
    static Order run(OrderService service, User user) {
        return service.place(user);
    }
}
