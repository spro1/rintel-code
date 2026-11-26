package com.acme.order;

import com.acme.user.User;

public interface OrderService {
    Order place(User user);
}
